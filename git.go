package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	log "github.com/sirupsen/logrus"
)

type git struct {
	root            string
	defaultUpstream string
}

func (g *git) GetBranch() ([]byte, error) {
	proc := exec.Command("git", "branch", "--show-current")
	return proc.CombinedOutput()
}

func (g *git) GetSha() ([]byte, error) {
	/*
	   This does not check if the commit is dirty.
	*/
	proc := exec.Command("git", "rev-parse", "HEAD")
	return proc.CombinedOutput()
}

func (g *git) GetWorkingHash() ([]byte, error) {
	/*
		A SHA based on both the git commit and any "dirty"
		changes made since that commit, whether staged or not.
	*/
	proc := exec.Command("git", "diff", "HEAD")
	out, err := proc.CombinedOutput()
	if err != nil {
		log.Fatal(err)
	}
	hasher := sha256.New()
	hasher.Write(out)
	proc = exec.Command("git", "rev-parse", "HEAD")
	out, err = proc.CombinedOutput()
	if err != nil {
		return []byte{}, err
	}
	hasher.Write(out)
	return hasher.Sum(nil), nil
}

func (g *git) GetChangedPaths(sinceRef string) Paths {
	// combine `git diff xxx...` and `git ls-files --modified`
	result := make(Paths)
	proc_diff := exec.Command("git", "diff", fmt.Sprintf("%v...", sinceRef), "--stat", "--name-only")
	diff_output, err := proc_diff.CombinedOutput()
	if err != nil {
		log.Warn(err)
		return result
	}
	proc_ls := exec.Command("git", "ls-files", "--modified")
	ls_output, err := proc_ls.CombinedOutput()
	if err != nil {
		log.Warn(err)
		return result
	}
	for _, path := range strings.Split(string(diff_output)+string(ls_output), "\n") {
		if len(path) > 0 {
			result[path] = Member
		}
	}

	return result
}

func (g *git) IsIgnored(path string) bool {
	proc := exec.Command("git", "check-ignore", path)
	err := proc.Run()
	return err == nil
}

func (g *git) GetRoot() string {
	result, err := filepath.EvalSymlinks(g.root)
	if err != nil {
		log.Fatal(err)
	}
	return result
}

func createGit(pathInRepo string) (*git, error) {
	path, err := filepath.Abs(pathInRepo)
	if err != nil {
		return nil, err
	}
	for {
		if path == "/" {
			return nil, fmt.Errorf("No git project root was found, starting at %v", pathInRepo)
		}
		if PathExists(filepath.Join(path, ".git")) {
			// os.Chdir(g.GetRoot())
			defaultUpstream := calculateDefaultUpstream(path)
			return &git{root: path, defaultUpstream: defaultUpstream}, nil
		}
		path = filepath.Dir(path)
	}
}

func calculateDefaultUpstream(root string) string {
	candidates := []string{"origin/main", "origin/master"}
	if env := os.Getenv("GIT_DEFAULT_UPSTREAM"); len(env) > 0 {
		candidates = []string{env}
	}
	args := append([]string{"branch", "--list", "--remote"}, candidates...)
	proc := exec.Command("git", args...)
	proc.Dir = root
	output, err := proc.CombinedOutput()
	if err != nil {
		return ""
	}
	found := strings.Split(string(output), "\n")
	if len(found[0]) == 0 {
		log.Warningf("No upstream branches could be detected from %v, such as 'origin/main'. You will need to provide a valid branch name to use via GIT_DEFAULT_UPSTREAM", candidates)
		return ""
	}
	return strings.TrimSpace(found[0])
}

type Git interface {
	GetBranch() (branch []byte, err error)
	GetWorkingHash() (ref []byte, err error)
	GetChangedPaths(sinceRef string) Paths
	IsIgnored(path string) bool
	GetRoot() (path string)
	DetectBranchChange(notify chan []byte)
	GetDefaultUpstream() string
}

func (g *git) GetDefaultUpstream() string {
	return g.defaultUpstream
}

func (g *git) DetectBranchChange(notify chan []byte) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	branch, err := g.GetBranch()
	if err != nil {
		log.Fatal(err)
	}
	notify <- branch
	watcher.Add(filepath.Join(g.root, ".git"))
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) {
				// flush any extra events which have accrued
			loop2:
				for {
					select {
					case <-watcher.Events:
					default:
						break loop2
					}
				}
				time.Sleep(time.Millisecond * 100)
				newBranch, err := g.GetBranch()
				if err != nil {
					log.Fatal(err)
				}
				if !bytes.Equal(newBranch, branch) {
					branch = newBranch
					notify <- branch
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Error(err)
		}
	}
}
