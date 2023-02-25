package pyast

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	file "github.com/nicois/pytestw/file"
	"github.com/nicois/pytestw/pytest"
	set "github.com/nicois/pytestw/set"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
)

type Classes = set.StringSet

func MakeClasses() Classes {
	return Classes(set.Create())
}

type node struct {
	importers Classes
}
type tree struct {
	root  string
	nodes map[string]node // maps class
	mutex *sync.Mutex
}

type trees []tree

/*
fixme: use a channel to collect the classes from goroutines
*/

type seen struct {
	nodes Classes
	mutex *sync.Mutex
}

func (s *seen) AddClass(class string) bool {
	// returns true if it was added (ie: not there already)
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, ok := s.nodes[class]; ok {
		return false
	}
	s.nodes.Add(class)
	return true
}

func (t *tree) getClassDependencies(wg *sync.WaitGroup, s seen, class string, result chan string) {
	/*
	   If this class has dependencies, emit them out, and schedule recursive
	   checking unless they are already seen
	*/
	defer wg.Done()
	if node, ok := t.nodes[class]; ok {
		result <- class
		for dep := range node.importers {
			if s.AddClass(dep) {
				wg.Add(1)
				go t.getClassDependencies(wg, s, dep, result)
			}
		}
	}
}

func (t *trees) GetDependees(paths file.Paths) (file.Paths, error) {
	// this is super fast; no need for goroutines
	result := file.CreatePaths()
	for _, tree := range *t {
		deps, err := tree.GetDependees(paths)
		if err != nil {
			return result, err
		}
		result.Union(deps)
	}
	return result, nil
}

func (t *tree) GetDependees(paths file.Paths) (file.Paths, error) {
	var wg sync.WaitGroup
	dependees := make(chan string, 10)
	seen := seen{mutex: new(sync.Mutex), nodes: make(Classes)}
	result := file.CreatePaths()
	for path := range paths {
		path, err := filepath.Abs(path)
		if err != nil {
			return result, err
		}
		if !strings.HasPrefix(path, t.root) {
			log.Debugf("%v is not contained within %v. Ignoring it.", path, t.root)
			continue
		}
		class := pytest.PathToClass(path[len(t.root)+1:])
		wg.Add(1)
		go t.getClassDependencies(&wg, seen, class, dependees)
	}
	go func() {
		wg.Wait()
		close(dependees)
	}()
	for class := range dependees {
		if _, ok := result[class]; !ok {
			result.Add(pytest.ClassToPath(t.root, class))
		}
	}
	return result, nil
}

type depPair struct {
	importerClass string
	importedClass string // ie: what is imported by the importer
}

func BuildTrees(pythonRoots file.Paths) *trees {
	var wg sync.WaitGroup
	c := make(chan tree)
	for pythonRoot := range pythonRoots {
		log.Infof("Checking under python root %v", pythonRoot)
		wg.Add(1)
		go BuildTree(&wg, c, pythonRoot)
	}
	go func() {
		wg.Wait()
		close(c)
	}()
	result := make(trees, 0)
	for t := range c {
		result = append(result, t)
	}
	if destinationFilename := os.Getenv("PYAST_DUMP_LOCATION"); destinationFilename != "" {
		destination, err := os.Create(destinationFilename)
		if err == nil {
			defer destination.Close()
			for _, tree := range result {
				destination.WriteString(fmt.Sprintf("Tree root: %v; %v nodes:\n", tree.root, len(tree.nodes)))
				for importee, node := range tree.nodes {
					destination.WriteString(fmt.Sprintf("\t%v is imported by: %v\n", importee, node.importers))
				}
				destination.WriteString(fmt.Sprintf("\n\n"))
			}
		} else {
			log.Warningf("Could not open %v so not creating a dump file: %v", destinationFilename, err)
		}

	}
	return &result
}

func BuildTree(pwg *sync.WaitGroup, c chan tree, pythonRoot string) {
	pythonRoot, err := filepath.Abs(pythonRoot)
	if err != nil {
		log.Fatal(err)
	}
	var wg sync.WaitGroup
	depPairs := make(chan depPair)
	wg.Add(1)
	go buildDependencies(&wg, pythonRoot, depPairs)
	go func() {
		wg.Wait()
		close(depPairs)
	}()
	nodes := make(map[string]node)
	numberOfPairs := 0
	for depPair := range depPairs {
		numberOfPairs++
		n, ok := nodes[depPair.importedClass]
		if !ok {
			n = node{importers: MakeClasses()}
			nodes[depPair.importedClass] = n
		}
		n.importers.Add(depPair.importerClass)
	}
	c <- tree{root: pythonRoot, nodes: nodes}
	pwg.Done()
}

func buildDependencies(wg *sync.WaitGroup, pythonRoot string, depPairs chan depPair) {
	/*
		f, err := os.Create("cpu.prof")
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close() // error handling omitted for example
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	*/

	// this controls the maximum number of files being read
	// concurrently, to avoid "too many open files" errors
	sem := semaphore.NewWeighted(10)
	filepath.WalkDir(pythonRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Fatalf("Cannot scan %v - not readable", path)
		}
		if d.IsDir() {
			if path != pythonRoot && !file.FileExists(filepath.Join(path, "__init__.py")) {
				log.Debugf("%v does not contain __init__.py so skipping.", path)
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".py") {
			wg.Add(1)
			go scan(wg, depPairs, sem, pythonRoot, path)
		}
		return nil
	})
	wg.Done()
}

func stripComments(source string) string {
	expressions := []*regexp.Regexp{regexp.MustCompile(`(?m)'''.+?'''`), regexp.MustCompile(`(?m)""".+?"""`), regexp.MustCompile(`.+'''`)}
	for _, re := range expressions {
		source = re.ReplaceAllLiteralString(source, "")
	}
	re := regexp.MustCompile(`(?m)(\".*?\"|\'.*?\')|(#[^\r\n]*$)`)
	result := re.ReplaceAllStringFunc(source, func(s string) string {
		if strings.HasPrefix(s, "'") || strings.HasPrefix(s, "\"") {
			return s
		}
		return ""
	})
	return strings.TrimSpace(result)
}

func scan(wg *sync.WaitGroup, depPairs chan depPair, sem *semaphore.Weighted, root string, path string) {
	defer wg.Done()
	if !strings.HasPrefix(path, root) {
		log.Fatalf("%v does not start with %v, so cannot calculate the module path.", path, root)
	}
	class := pytest.PathToClass(path[len(root)+1:])

	reImport := regexp.MustCompile(`(?m)^(?:from[ ]+(\S+)[ ]+)?import[ ]+([^\s\(]+|\([^\)]+?\))(?:[ ]+as[ ]+\S+)?[ ]*$`)
	sem.Acquire(context.Background(), 1)
	content, err := file.ReadBytes(path)
	sem.Release(1)
	if err != nil {
		log.Fatalf("While reading %v: %v", path, err)
	}
	for _, match := range reImport.FindAllStringSubmatch(stripComments(string(content)), -1) {
		// log.Debugf("%v ---- %v", match[1], match[2])
		packageName := match[1]
		var names []string
		if strings.Contains(match[2], ",") {
			names = strings.Split(strings.Trim(match[2], "()"), ",")
		} else {
			names = []string{match[2]}
		}
		// FIXME resolve '..'
		if strings.Contains(packageName, "..") {
			log.Fatalf("Cannot yet support .. in import line %v of %v:\n%v", packageName, path, match[0])
		}
		if strings.HasPrefix(packageName, ".") {
			// ie: "from .foo import bar"
			if strings.LastIndex(class, ".") == -1 {
				log.Fatalf("Looking for . in %v (class) with %v (packageName) and %q (match)", class, packageName, match)
			}
			parentClass := class[:strings.LastIndex(class, ".")]
			if len(packageName) > 1 {
				packageName = parentClass + packageName
			} else {
				packageName = parentClass
			}
		}
		// add the parent as a dep, as e.g. "from foo import bar" might mean
		// foo is a module, or foo.bar
		for _, name := range names {
			var dep string
			if packageName == "" {
				dep = strings.TrimSpace(name)
				// log.Debugf("'%v' --> '%v'", packageName, dep)
			} else {
				dep = fmt.Sprintf("%v.%v", packageName, strings.TrimSpace(name))
			}
			depPairs <- depPair{importerClass: class, importedClass: dep}
			if lastDotIndex := strings.LastIndex(dep, "."); lastDotIndex > 0 {
				depPairs <- depPair{importerClass: class, importedClass: dep[:lastDotIndex]}
			}
			// log.Debugf("'%v' --> '%v'", packageName, dep)
		}

	}
}

func CalculatePythonRoots(paths file.Paths) file.Paths {
	result := file.CreatePaths()
	for path := range paths {
		if !strings.HasSuffix(path, ".py") {
			log.Infof("%v does not appear to be a python module. Ignoring it.", path)
		}
		absolutePath, err := filepath.Abs(path)
		if err != nil {
			log.Info(err)
			continue
		}
		dir := filepath.Dir(absolutePath)
		for {
			if !file.FileExists(filepath.Join(dir, "__init__.py")) {
				break
			}
			dir = filepath.Dir(dir)
			if !file.DirExists(dir) {
				log.Fatalf("%v does not exist, while trying to find top-level of %v", dir, path)
			}
		}
		result.Add(dir)
	}
	return result
}
