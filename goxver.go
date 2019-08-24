package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Exit codes
const (
	ExitOk   = 0
	ExitFail = 1
)

// Constants to have less or no magic numbers
const (
	nameGoMod    = "go.mod"
	nameGoPath   = "GOPATH"
	extGo        = ".go"
	dirChunkSize = 100
)

// Generator names
const (
	GenVersion   = "version"    // The most recent symver in format vX[.Y[.Z]] or X[.Y[.Z]] form tags
	GenTag       = "tag"        // The most recent tag
	GenHash      = "hash"       // The hash of the revision
	GenHashShort = "hash_short" // The short hash of the revision
	GenHashLong  = "hash_long"  // The long hash of the revision
	GenTime      = "time"       // The current time in format YYYY-MM-DD HH:MM:SS
)

// Target is the name and location of the variable to push some data into.
type Target struct {
	Var  string
	Pkg  string
	Path string
}

var (
	// The map of known target variable names and generators for them.
	// Variables names are case insensitive.
	KnownTargets = map[string]string{
		"BuildVersion":   GenVersion,
		"BuildSymVer":    GenVersion,
		"BuildTag":       GenTag,
		"BuildHash":      GenHash,
		"BuildHashShort": GenHashShort,
		"BuildHashLong":  GenHashLong,
		"BuildTime":      GenTime,
	}
)

// Regular expressions for parsing various things
var (
	reGoModPackage      = regexp.MustCompile("^module (.+)$")
	reCommentBlockOpen  = regexp.MustCompile(`^\s*\\\*`)
	reCommentBlockClose = regexp.MustCompile(`\*\\\\s*$`)
	rePackageName       = regexp.MustCompile(`^\s*package\s+(\S+)`)
	reKnownTargets      *regexp.Regexp
)

// Command line options
var (
	rootDir string // The root directory of project (-d path)
	verbose bool   // Enable verbose mode (-v)
)

func init() {
	flag.StringVar(&rootDir, "d", ".", "The root directory of the project")
	flag.BoolVar(&verbose, "v", false, "Enable verbose mode")
}

func main() {
	var err error

	// Pretty print panic
	defer func() {
		if r := recover(); r != nil {
			_, _ = fmt.Fprintln(os.Stderr, r)
			os.Exit(ExitFail)
		}
	}()

	// Prepare
	flag.Parse()

	if dir, err := filepath.Abs(rootDir); err != nil {
		panic("failed to get absolute path: " + err.Error())
	} else {
		rootDir = dir
	}

	var knownTargetsList string
	for key, _ := range KnownTargets {
		if len(knownTargetsList) > 0 {
			knownTargetsList += "|"
		}
		knownTargetsList += key
	}
	if reKnownTargets, err = regexp.Compile("\b(?i)(" + knownTargetsList + ")\\s+string\\b"); err != nil {
		panic("failed to compile know targets parser: " + err.Error())
	}

	// Generate version
	pkg, err := rootPkg(rootDir)
	if err != nil {
		panic("failed to find root package: " + err.Error())
	} else if len(pkg) == 0 {
		panic("failed to find root package")
	}

	msg("Root package is %s\n", pkg)

	os.Exit(ExitOk)
}

// msg formats and prints message to STDERR if verbose mode is enabled
func msg(s string, args ...interface{}) {
	if verbose {
		_, _ = fmt.Fprintf(os.Stderr, s, args...)
	}
}

// rootPkg finds the root package of the project in the order
// 1. try to read it from go.mod file
// 2. extract it from the path given
func rootPkg(path string) (pkg string, err error) {
	pkg, err = readPkgFromMod(path)
	if err == nil && len(pkg) == 0 {
		pkg = makePkgFromPath(path)
	}
	return
}

// readPkgFromMod reads package from go.mod file if it exists.
func readPkgFromMod(path string) (string, error) {
	file, err := os.Open(filepath.Join(path, nameGoMod))
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return "", err
	}
	defer file.Close()

	var pkg string
	err = iterTextLines(file, func(line []byte) error {
		if matches := reGoModPackage.FindSubmatch(line); len(matches) > 0 {
			pkg = string(matches[len(matches)-1])
			return StopReading
		}
		return nil
	})

	return pkg, err
}

// makePkgFromPath makes package from the path given and based on GOPATH env.
func makePkgFromPath(path string) string {
	srcPath := filepath.Join(os.Getenv(nameGoPath), "src")
	if index := strings.Index(path, srcPath); index >= 0 {
		// Strip GOAPTH from path
		path = path[index+len(srcPath):]
		sep := string(filepath.Separator)
		if strings.HasPrefix(path, sep) {
			path = path[len(sep):]
		}
		if strings.HasSuffix(path, sep) {
			path = path[:len(path)-len(sep)]
		}
	}
	return path
}

// StopReading is the special case for text stream iterator which means stop further reading.
var StopReading = errStopReading{}

type errStopReading struct{}

func (errStopReading) Error() string { return "stop reading" }

// iterTextLines treats the reader as a text stream and reads it line by line.
// Each read line is passed into the processor. If the processor returns a non-nil error the further
// reading is stopped. The error returned from the processor propagate further unless it is StopReading error.
func iterTextLines(reader io.ReadCloser, processor func([]byte) error) error {
	textStream := bufio.NewReader(reader)
	for {
		// Read the next line
		line, _, err := textStream.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		// Process the line
		err = processor(line)
		if err != nil {
			if err == StopReading {
				break
			}
			return err
		}
	}

	return nil
}

// findAllTargets scans the file tree and finds locations of variables to push version info into.
func findAllTargets(path string) ([]Target, error) {
	var (
		mut     sync.Mutex
		targets []Target
		errs    []string
		wg      sync.WaitGroup
	)

	pushTargets := func(t []Target) {
		mut.Lock()
		targets = append(targets, t...)
		mut.Unlock()
	}
	pushErr := func(info os.FileInfo, err error) {
		mut.Lock()
		if info != nil {
			errs = append(errs, fmt.Sprintf("failed to scan %s: %s", info.Name(), err.Error()))
		} else {
			errs = append(errs, err.Error())
		}
		mut.Unlock()
	}

	var processor func(info os.FileInfo) error
	processor = func(info os.FileInfo) error {
		// Launch a new directory scanner if the file is of dir type or
		// scan for target variables if that is a *.go file.
		if info.IsDir() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := scanDir(info.Name(), processor); err != nil {
					pushErr(info, err)
				}
			}()
		} else if filepath.Ext(info.Name()) == extGo {
			if targets, err := scanTargets(info.Name()); err != nil {
				pushErr(info, err)
			} else if len(targets) > 0 {
				pushTargets(targets)
			}
		}

		return nil
	}

	// Start scanning form the root directory
	wg.Add(1)
	if err := scanDir(path, processor); err != nil {
		pushErr(nil, err)
	}
	wg.Done()
	wg.Wait()

	// Return what we have
	if len(errs) > 0 {
		return nil, fmt.Errorf("failed to scan file tree\n%s", strings.Join(errs, "\n"))
	}
	return targets, nil
}

// scanDir iterates over all files in the directory and runs the processor on the each.
func scanDir(path string, processor func(os.FileInfo) error) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()

	for {
		files, err := dir.Readdir(dirChunkSize)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		for _, file := range files {
			if err = processor(file); err != nil {
				return err
			}
		}
	}

	return nil
}

// scanTargets scans the file for target variables.
func scanTargets(path string) ([]Target, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var (
		targets            []Target
		pkg                string
		insideCommentBlock bool
	)
	err = iterTextLines(file, func(line []byte) error {
		if reCommentBlockOpen.Match(line) {
			insideCommentBlock = true
		}
		if reCommentBlockClose.Match(line) {
			insideCommentBlock = false
		}
		if !insideCommentBlock {
			if matches := rePackageName.FindSubmatch(line); len(matches) > 0 {
				pkg = string(matches[len(matches)-1])
			}
		}
		return nil
	})

	return targets, nil
}
