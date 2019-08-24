package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	typeString   = "string"
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
	Var string
	Pkg string
	Gen string
}

var (
	// The map of known target variable names and generators for them.
	// Variables names are case insensitive.
	knownTargetParser = map[string]string{
		"Version":        GenVersion,
		"BuildVersion":   GenVersion,
		"SymVer":         GenVersion,
		"BuildSymVer":    GenVersion,
		"GitTag":         GenTag,
		"BuildTag":       GenTag,
		"GitHash":        GenHash,
		"BuildHash":      GenHash,
		"GitHashShort":   GenHashShort,
		"BuildHashShort": GenHashShort,
		"GitHashLong":    GenHashLong,
		"BuildHashLong":  GenHashLong,
		"BuildTime":      GenTime,
	}
)

// Regular expressions for parsing various things
var (
	reGoModPackage = regexp.MustCompile("^module (.+)$")
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

	// Find which is the root package
	pkg, err := rootPkg(rootDir)
	if err != nil {
		panic("failed to find root package: " + err.Error())
	} else if len(pkg) == 0 {
		panic("failed to find root package")
	}

	// Find all target variables which should be substituted
	targets, err := findAllTargets(rootDir)
	if err != nil {
		panic("failed to scan targets: " + err.Error())
	} else {
		// Fix target packages
		for i := 0; i < len(targets); i++ {
			stripped := stripHeadPath(targets[i].Pkg, rootDir)
			if len(stripped) > 0 {
				targets[i].Pkg = strings.ReplaceAll(pkg+"/"+stripped, string(filepath.Separator), "/")
			} else {
				targets[i].Pkg = strings.ReplaceAll(pkg, string(filepath.Separator), "/")
			}
		}
	}

	// Dump debug info
	msg("Root package is %s\n", pkg)
	if len(targets) > 0 {
		for _, t := range targets {
			msg("Target %s.%s with %s generator\n", t.Pkg, t.Var, t.Gen)
		}
	} else {
		msg("No targets found\n")
	}

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
	return stripHeadPath(path, srcPath)
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
func findAllTargets(dir string) ([]Target, error) {
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

	var processor func(dir string, info os.FileInfo) error
	processor = func(dir string, info os.FileInfo) error {
		fullPath := filepath.Join(dir, info.Name())

		// Launch a new directory scanner if the file is of dir type or
		// scan for target variables if that is a *.go file.
		if info.IsDir() {
			// Skip parsing directories starting from dot
			if !strings.HasPrefix(info.Name(), ".") {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := scanDir(fullPath, processor); err != nil {
						pushErr(info, err)
					}
				}()
			}
		} else if filepath.Ext(info.Name()) == extGo {
			if targets, err := scanTargets(fullPath); err != nil {
				pushErr(info, err)
			} else if len(targets) > 0 {
				pushTargets(targets)
			}
		}

		return nil
	}

	// Start scanning form the root directory
	wg.Add(1)
	if err := scanDir(dir, processor); err != nil {
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
func scanDir(path string, processor func(string, os.FileInfo) error) error {
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
			if err = processor(path, file); err != nil {
				return err
			}
		}
	}

	return nil
}

// scanTargets scans the file for target variables.
func scanTargets(path string) ([]Target, error) {
	var targets []Target

	// Build the AST of the file
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.DeclarationErrors)
	if err != nil {
		return nil, err
	}

	// Extract all matched targets
	for _, decl := range file.Decls {
		// Skip the declaration unless that is var
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		if gen.Tok != token.VAR {
			continue
		}

		// Process variable declarations only
		for _, spec := range gen.Specs {
			// Ignore non value specs
			val, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}

			// We can populate only strings so far. So skip non-string variables.
			if ident, ok := val.Type.(*ast.Ident); ok {
				if ident.Name != typeString {
					continue
				}
			} else {
				// Skip the declaration because the type is not known.
				// I assume standard types are always populated as *ast.Ident.
				continue
			}

			// Add to found targets all variables with known names.
			for _, name := range val.Names {
				if gen := findNameGen(name.Name); len(gen) > 0 {
					pkg := filepath.Join(
						filepath.Dir(filepath.Dir(path)), // remove the 2nd last dir name
						file.Name.Name,                   // and replace it with the package name
					)
					targets = append(targets, Target{
						Var: name.Name,
						Pkg: pkg,
						Gen: gen,
					})
				}
			}
		}
	}

	return targets, nil
}

// findNameGen returns the generator class for the name if it's known.
func findNameGen(name string) string {
	for key, value := range knownTargetParser {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

// stripHeadPath removes from the path the same heading path.
func stripHeadPath(path, heading string) string {
	if index := strings.Index(path, heading); index >= 0 {
		path = path[index+len(heading):]
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
