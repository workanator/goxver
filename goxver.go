/*
goxver is the tool for generating LDFLAGS argument with version information populated.
The tool works only with git repositories.

	Usage:
		go build -ldflags `goxver` main.go

Original idea and implementation by Andrew "workanator" Bashkatov.
Licensed under MIT license.
*/
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	git "gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/storer"
)

// Exit codes
const (
	ExitOk   = 0
	ExitFail = 1
)

// Constants to have less or no magic numbers
const (
	currentDir        = "."
	defaultConfigName = ".goxver"
	goModName         = "go.mod"
	goPathEnv         = "GOPATH"
	goSourceSuffix    = ".go"
	goTestSuffix      = " _test.go"
	dirChunkSize      = 100
	typeString        = "string"
	timeFormat        = "2006-01-02_15:04:05_Z07:00"
	versionPrefix     = "v"
	versionSeparator  = "."
	gitDirName        = ".git"
	srcDirName        = "src"
	mapSeparator      = ","
	mapAssignment     = "="
)

// Generator names
const (
	GenVersion   = "version"    // The most recent symver in format vX[.Y[.Z]] or X[.Y[.Z]] form tags
	GenTag       = "tag"        // The most recent tag
	GenHashShort = "hash_short" // The short hash of the revision
	GenHashLong  = "hash_long"  // The long hash of the revision
	GenTime      = "time"       // The current time in format YYYY-MM-DD_HH:MM:SS_Z
)

var ValidGens = []string{
	GenVersion,
	GenTag,
	GenHashShort,
	GenHashLong,
	GenTime,
}

// Target is the name and location of the variable to push some data into.
type Target struct {
	Var string
	Pkg string
	Gen string
}

// TargetMap maps targets to generators.
type TargetMap map[string]string

var (
	// The map of known target variable names and generators for them.
	// Variables names are case insensitive.
	targetDict = TargetMap{}
)

// Regular expressions for parsing various things
var (
	reGoModPackage = regexp.MustCompile("^module (.+)$")
	reVersion      = regexp.MustCompile(`^v?\d+(?:\.\d+){0,2}`)
)

// Command line options
var (
	rootDir     string // The root directory of project (-d path)
	configPath  string // The path to the configuration file (-c path)
	configMap   string // The mapping (-m mapping)
	doubleQuote bool   // Put generated values into double quotes (-qq)
	verbose     bool   // Enable verbose mode (-v)
)

func init() {
	flag.StringVar(&rootDir, "d", currentDir, "The root directory of the project")
	flag.StringVar(&configPath, "c", "", "The path to the configuration file")
	flag.StringVar(&configMap, "m", "", "The mapping")
	flag.BoolVar(&doubleQuote, "qq", false, "Double quote values")
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

	// Exit with error if the directory i snot found
	if !fileExists(rootDir) {
		panic("path does not exist")
	}
	// Exit silently if the git repository does not exists
	if !fileExists(filepath.Join(rootDir, gitDirName)) {
		msg("No git repository found\n")
		os.Exit(ExitOk)
	}

	// Load the configuration file
	if len(configPath) == 0 {
		configPath = findConfigFile(rootDir)
	}
	if len(configPath) > 0 {
		msg("Loading configuration from %s\n", configPath)
		if err := readConfigFile(configPath); err != nil {
			panic("failed to read configuration file: " + err.Error())
		}
	} else {
		msg("Use no configuration file\n")
	}

	if len(configMap) > 0 {
		m, err := parseTargetMapping(configMap)
		if err != nil {
			panic("failed to parse mapping: " + err.Error())
		}

		for t, g := range m {
			targetDict[t] = g
		}
	}

	if len(targetDict) > 0 {
		msg("Target mappings:\n")
		for t, g := range targetDict {
			msg("  - %s = %s\n", t, g)
		}
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
		// Do not panic of errors while parsing source code because
		// here can be issued files in the work tree but they maybe not required for build.
		// Also having goxver failing on source will fail the command the tool can
		// be embedded into.
		msg("failed to scan targets: " + err.Error() + "\n")
	}

	// Fix target packages
	for i := 0; i < len(targets); i++ {
		stripped := stripHeadPath(targets[i].Pkg, rootDir)
		if len(stripped) > 0 {
			targets[i].Pkg = strings.ReplaceAll(pkg+"/"+stripped, string(filepath.Separator), "/")
		} else {
			targets[i].Pkg = strings.ReplaceAll(pkg, string(filepath.Separator), "/")
		}
	}

	// Dump debug info
	msg("Root package is %s\n", pkg)
	if len(targets) > 0 {
		msg("Targets:\n")
		for _, t := range targets {
			msg("  - %s.%s with %s generator\n", t.Pkg, t.Var, t.Gen)
		}
	} else {
		msg("No targets found\n")
	}

	// Skip further processing if not targets found.
	if len(targets) == 0 {
		os.Exit(ExitOk)
	}

	// Open the git repository and generate LDFLAGS argment value.
	repo, err := git.PlainOpen(rootDir)
	if err != nil {
		panic("failed to open git repository: " + err.Error())
	}

	value, err := generateLDFlags(repo, targets)
	if err != nil {
		panic("failed to generate LDFLAGS: " + err.Error())
	}

	// Print LDFLAGS argument at last, yay!
	fmt.Print(value)
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
	file, err := os.Open(filepath.Join(path, goModName))
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
	srcPath := filepath.Join(os.Getenv(goPathEnv), "src")
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
		} else if filepath.Ext(info.Name()) == goSourceSuffix && !strings.HasSuffix(info.Name(), goTestSuffix) {
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

	// Find the targets through the top-level declarations and
	// add to found targets all variables with known names.
	for _, val := range onlyStringValues(onlyVarDecls(file.Decls)) {
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

	return targets, nil
}

// onlyVarDecls filters the list of declarations leaving only GenDecl of VAR type.
func onlyVarDecls(decls []ast.Decl) (vars []*ast.GenDecl) {
	for _, decl := range decls {
		if gen, ok := decl.(*ast.GenDecl); ok {
			if gen.Tok == token.VAR {
				vars = append(vars, gen)
			}
		}
	}
	return
}

// onlyStringValues flatten the list of variable declarations leaving only string variables.
func onlyStringValues(decls []*ast.GenDecl) (values []*ast.ValueSpec) {
	for _, decl := range decls {
		for _, spec := range decl.Specs {
			// Ignore non-value specs
			val, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Leave only string variables
			if ident, ok := val.Type.(*ast.Ident); ok {
				if ident.Name == typeString {
					values = append(values, val)
				}
			}
		}
	}
	return
}

// findNameGen returns the generator class for the name if it's known.
func findNameGen(name string) string {
	for key, value := range targetDict {
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

// generateLDFlags generates LDFLAGS for targets found with the git repository info.
func generateLDFlags(repo *git.Repository, targets []Target) (string, error) {
	flags := make([]string, 0, len(targets))
	for _, target := range targets {
		var (
			value string
			err   error
		)
		switch target.Gen {
		case GenVersion:
			value, err = readGitLatestVersion(repo)
		case GenTag:
			value, err = readGitLatestTag(repo)
		case GenHashShort, GenHashLong:
			if value, err = readGitHEAD(repo); err == nil {
				if target.Gen == GenHashShort {
					value = value[:7]
				}
			}
		case GenTime:
			value = generateTime()
		}
		if err != nil {
			return "", err
		}
		if len(value) > 0 {
			flags = append(flags, fmt.Sprintf("-X %s.%s=%s", target.Pkg, target.Var, value))
		}
	}

	return strings.Join(flags, " "), nil
}

// readGitLatestVersion returns the newest version tag from the git repository.
func readGitLatestVersion(repo *git.Repository) (string, error) {
	tags, err := repo.Tags()
	if err != nil {
		return "", err
	}
	defer tags.Close()

	// Find all versions and returns the newest.
	versions, err := versionsFromTags(tags)
	if err != nil {
		return "", err
	}
	if len(versions) > 0 {
		return versions[0].String(), nil
	}
	return "", nil
}

// readGitLatestTag returns the latest tag from the git repository.
func readGitLatestTag(repo *git.Repository) (string, error) {
	tags, err := repo.Tags()
	if err != nil {
		return "", err
	}
	defer tags.Close()

	ref, err := tags.Next()
	if err != nil {
		if err == io.EOF {
			err = nil
		}
		return "", err
	}
	if ref != nil {
		return quoteValue(ref.Name().Short()), nil
	}

	return "", nil
}

// readGitHEAD returns the hash of the HEAD of the git repository.
func readGitHEAD(repo *git.Repository) (string, error) {
	head, err := repo.Head()
	if err != nil {
		return "", err
	}
	return head.Hash().String(), nil
}

// generateTime formats the current time.
func generateTime() string {
	return time.Now().Format(timeFormat)
}

// quoteValue quotes the value with double or single quotes based on the doubleQuote option.
func quoteValue(s string) string {
	if doubleQuote {
		return `"` + s + `"`
	}
	return "'" + s + "'"
}

// Version is a numeric representation semantic version.
type Version struct {
	Prefix              string
	Major, Minor, Build int
}

// String composes a string representation of the version in symver format.
func (v Version) String() string {
	return fmt.Sprintf("%s%d.%d.%d", v.Prefix, v.Major, v.Minor, v.Build)
}

// Less tests if the version is less than the other.
func (v Version) Less(other Version) bool {
	if v.Major < other.Major {
		return true
	} else if v.Minor < other.Minor {
		return true
	} else if v.Build < other.Build {
		return true
	}
	return false
}

// parseVersion parses the strings and makes a Version instance from it.
// The function assumes the input value is in valid symver format w/ or w/o heading v.
func parseVersion(s string) (v Version) {
	if strings.HasPrefix(s, versionPrefix) {
		s = s[len(versionPrefix):]
		v.Prefix = versionPrefix
	}

	parts := strings.Split(s, versionSeparator)
	v.Major, _ = strconv.Atoi(parts[0])
	if len(parts) > 1 {
		v.Minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) > 2 {
		v.Build, _ = strconv.Atoi(parts[2])
	}
	return
}

// versionsFromTags makes the list of versions from the repository tags.
// The list returned is sorted descending.
func versionsFromTags(tags storer.ReferenceIter) (versions []Version, err error) {
	err = tags.ForEach(func(ref *plumbing.Reference) error {
		if reVersion.MatchString(ref.Name().Short()) {
			versions = append(versions, parseVersion(ref.Name().Short()))
		}
		return nil
	})
	if err == nil {
		sort.Slice(versions, func(i, j int) bool {
			return versions[j].Less(versions[i])
		})
	}
	return
}

// fileExists tests if the file at the path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// parseTargetMapping parses the line with target to generator mapping.
// Mapping must be in the format var=gen[,var=gen]* where
// - var is the name of variable
// - gen is the valid name of value generator (one of ValidGens)
// - the string can contain multiple maps separated by comma
func parseTargetMapping(s string) (m TargetMap, err error) {
	items := strings.Split(s, mapSeparator)
	m = make(TargetMap, len(items))
	for _, item := range items {
		parts := strings.Split(item, mapAssignment)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid mapping %s", item)
		}
		if !isValidGen(parts[1]) {
			return nil, fmt.Errorf("invalid generator %s", item)
		}
		m[parts[0]] = parts[1]
	}
	return m, nil
}

// isValidGen tests if the name of the generator is in valid set.
func isValidGen(s string) bool {
	for _, gen := range ValidGens {
		if s == gen {
			return true
		}
	}
	return false
}

// findConfigFile searches for the config file in the directories in the follow order
// 1. In the current directory.
// 2. In the project directory.
// 3. In the source directory under $GOPATH.
func findConfigFile(projectDir string) string {
	dirs := []string{
		currentDir,
		projectDir,
		filepath.Join(os.Getenv(goPathEnv), srcDirName),
	}
	for _, dir := range dirs {
		path := filepath.Join(dir, defaultConfigName)
		if info, err := os.Stat(path); err == nil {
			if !info.IsDir() {
				return path
			}
		}
	}
	return ""
}

// readConfigFile reads and parses the configuration file.
func readConfigFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	return iterTextLines(file, func(line []byte) error {
		m, err := parseTargetMapping(string(line))
		if err != nil {
			return err
		}

		for t, g := range m {
			targetDict[t] = g
		}

		return nil
	})
}
