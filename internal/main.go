// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// Package internal implements panicparse
//
// It is mostly useful on servers will large number of identical goroutines,
// making the crash dump harder to read than strictly necessary.
//
// Colors:
//  - Magenta: first goroutine to be listed.
//  - Yellow: main package.
//  - Green: standard library.
//  - Red: other packages.
//
// Bright colors are used for exported symbols.
package internal

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"regexp"
	"syscall"

	"github.com/maruel/panicparse/v2/internal/htmlstack"
	"github.com/maruel/panicparse/v2/stack"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/mgutz/ansi"
)

// resetFG is similar to ansi.Reset except that it doesn't reset the
// background color, only the foreground color and the style.
//
// That much for the "ansi" abstraction layer...
const resetFG = ansi.DefaultFG + "\033[m"

// defaultPalette is the default recommended palette.
var defaultPalette = Palette{
	EOLReset:           resetFG,
	RoutineFirst:       ansi.ColorCode("magenta+b"),
	CreatedBy:          ansi.LightBlack,
	Race:               ansi.LightRed,
	Package:            ansi.ColorCode("default+b"),
	SrcFile:            resetFG,
	FuncStdLib:         ansi.Green,
	FuncStdLibExported: ansi.ColorCode("green+b"),
	FuncMain:           ansi.ColorCode("yellow+b"),
	FuncOther:          ansi.Red,
	FuncOtherExported:  ansi.ColorCode("red+b"),
	Arguments:          resetFG,
}

func writeBucketsToConsole(out io.Writer, p *Palette, b []*stack.Bucket, pf pathFormat, needsEnv bool, filter, match *regexp.Regexp) error {
	if needsEnv {
		_, _ = io.WriteString(out, "\nTo see all goroutines, visit https://github.com/maruel/panicparse#gotraceback\n\n")
	}
	srcLen, pkgLen := calcBucketsLengths(b, pf)
	for _, e := range b {
		header := p.BucketHeader(e, pf, len(b) > 1)
		if filter != nil && filter.MatchString(header) {
			continue
		}
		if match != nil && !match.MatchString(header) {
			continue
		}
		_, _ = io.WriteString(out, header)
		_, _ = io.WriteString(out, p.StackLines(&e.Signature, srcLen, pkgLen, pf))
	}
	return nil
}

func writeGoroutinesToConsole(out io.Writer, p *Palette, goroutines []*stack.Goroutine, pf pathFormat, needsEnv bool, filter, match *regexp.Regexp) error {
	if needsEnv {
		_, _ = io.WriteString(out, "\nTo see all goroutines, visit https://github.com/maruel/panicparse#gotraceback\n\n")
	}
	srcLen, pkgLen := calcGoroutinesLengths(goroutines, pf)
	for _, e := range goroutines {
		header := p.GoroutineHeader(e, pf, len(goroutines) > 1)
		if filter != nil && filter.MatchString(header) {
			continue
		}
		if match != nil && !match.MatchString(header) {
			continue
		}
		_, _ = io.WriteString(out, header)
		_, _ = io.WriteString(out, p.StackLines(&e.Signature, srcLen, pkgLen, pf))
	}
	return nil
}

func getHTMLFooter(needsEnv bool) template.HTML {
	if needsEnv {
		return "To see all goroutines, visit <a href=https://github.com/maruel/panicparse#gotraceback>github.com/maruel/panicparse</a>"
	}
	return ""
}

func processInner(out io.Writer, p *Palette, s stack.Similarity, pf pathFormat, html string, filter, match *regexp.Regexp, c *stack.Snapshot, first bool) error {
	log.Printf("GOROOT=%s", c.RemoteGOROOT)
	log.Printf("GOPATH=%s", c.RemoteGOPATHs)
	needsEnv := len(c.Goroutines) == 1 && showBanner()
	// Bucketing should only be done if no data race was detected.
	if c.Goroutines[0].RaceAddr == 0 {
		b := c.Aggregate(s).Buckets
		if html == "" {
			return writeBucketsToConsole(out, p, b, pf, needsEnv, filter, match)
		}
		f, err := os.Create(html)
		if err != nil {
			return err
		}
		err = htmlstack.WriteBuckets(f, b, getHTMLFooter(needsEnv))
		if err2 := f.Close(); err == nil {
			err = err2
		}
		return err
	}
	// It's a data race.
	if html == "" {
		return writeGoroutinesToConsole(out, p, c.Goroutines, pf, needsEnv, filter, match)
	}
	f, err := os.Create(html)
	if err != nil {
		return err
	}
	err = htmlstack.WriteGoroutines(f, c.Goroutines, getHTMLFooter(needsEnv))
	if err2 := f.Close(); err == nil {
		err = err2
	}
	return err
}

// process copies stdin to stdout and processes any "panic: " line found.
//
// If html is used, a stack trace is written to this file instead.
func process(in io.Reader, out io.Writer, p *Palette, s stack.Similarity, pf pathFormat, parse, rebase bool, html string, filter, match *regexp.Regexp) error {
	opts := stack.DefaultOpts()
	if !rebase {
		opts.GuessPaths = false
		opts.AnalyzeSources = false
	}
	if !parse {
		opts.AnalyzeSources = false
	}
	for first := true; ; first = false {
		c, suffix, err := stack.ScanSnapshot(in, out, opts)
		if c != nil {
			// Process it even if an error occurred.
			if err1 := processInner(out, p, s, pf, html, filter, match, c, first); err == nil {
				err = err1
			}
		}
		if err == nil {
			// This means the whole buffer was not read, loop again.
			in = io.MultiReader(bytes.NewReader(suffix), in)
			continue
		}
		if len(suffix) != 0 {
			if _, err1 := out.Write(suffix); err == nil {
				err = err1
			}
		}
		if err == io.EOF {
			return nil
		}
		// Parts of the input will be lost.
		return err
	}
}

func showBanner() bool {
	if !showGOTRACEBACKBanner {
		return false
	}
	gtb := os.Getenv("GOTRACEBACK")
	return gtb == "" || gtb == "single"
}

// Main is implemented here so both 'pp' and 'panicparse' executables can be
// compiled. This is to work around the Perl Package manager 'pp' that is
// preinstalled on some OSes.
func Main() error {
	aggressive := flag.Bool("aggressive", false, "Aggressive deduplication including non pointers")
	parse := flag.Bool("parse", true, "Parses source files to deduct types; use -parse=false to work around bugs in source parser")
	rebase := flag.Bool("rebase", true, "Guess GOROOT and GOPATH")
	verboseFlag := flag.Bool("v", false, "Enables verbose logging output")
	filterFlag := flag.String("f", "", "Regexp to filter out headers that match, ex: -f 'IO wait|syscall'")
	matchFlag := flag.String("m", "", "Regexp to filter by only headers that match, ex: -m 'semacquire'")
	// Console only.
	fullPathArg := flag.Bool("full-path", false, "Print full sources path")
	relPathArg := flag.Bool("rel-path", false, "Print sources path relative to GOROOT or GOPATH; implies -rebase")
	noColor := flag.Bool("no-color", !isatty.IsTerminal(os.Stdout.Fd()) || os.Getenv("TERM") == "dumb", "Disable coloring")
	forceColor := flag.Bool("force-color", false, "Forcibly enable coloring when with stdout is redirected")
	// HTML only.
	html := flag.String("html", "", "Output an HTML file")
	flag.Parse()

	log.SetFlags(log.Lmicroseconds)
	if !*verboseFlag {
		log.SetOutput(ioutil.Discard)
	}

	var err error
	var filter *regexp.Regexp
	if *filterFlag != "" {
		if filter, err = regexp.Compile(*filterFlag); err != nil {
			return err
		}
	}

	var match *regexp.Regexp
	if *matchFlag != "" {
		if match, err = regexp.Compile(*matchFlag); err != nil {
			return err
		}
	}

	s := stack.AnyPointer
	if *aggressive {
		s = stack.AnyValue
	}

	var out io.Writer = os.Stdout
	p := &defaultPalette
	if *html == "" {
		if *noColor && !*forceColor {
			p = &Palette{}
		} else {
			out = colorable.NewColorableStdout()
		}
	}

	var in *os.File
	switch flag.NArg() {
	case 0:
		in = os.Stdin
		// Explicitly silence SIGQUIT, as it is useful to gather the stack dump
		// from the piped command..
		signals := make(chan os.Signal)
		go func() {
			for {
				<-signals
			}
		}()
		signal.Notify(signals, os.Interrupt, syscall.SIGQUIT)

	case 1:
		// Do not handle SIGQUIT when passed a file to process.
		name := flag.Arg(0)
		if in, err = os.Open(name); err != nil {
			return fmt.Errorf("did you mean to specify a valid stack dump file name? "+wrap, err)
		}
		defer in.Close()

	default:
		return errors.New("pipe from stdin or specify a single file")
	}
	pf := basePath
	if *fullPathArg {
		if *relPathArg {
			return errors.New("can't use both -full-path and -rel-path")
		}
		pf = fullPath
	} else if *relPathArg {
		pf = relPath
		*rebase = true
	}
	return process(in, out, p, s, pf, *parse, *rebase, *html, filter, match)
}
