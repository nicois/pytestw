package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type Version interface {
	Current() []byte
	NotifyOnChange(initial []byte, onChanged chan []byte) int
	CancelNotifyOnChange(i int)
}

func WaitForCurrent(v Version) []byte {
	c := make(chan []byte)
	defer v.CancelNotifyOnChange(v.NotifyOnChange([]byte(""), c))
	return (<-c)
}

/* This type is used when you don't really care about being
   notified on changes; it's sufficient to be able to get
   the current value on demand.
*/
type Reactive func() ([]byte, error)

func (r Reactive) Current() []byte {
	result, err := r()
	if err != nil {
		return nil
	}
	return result
}

func (r Reactive) NotifyOnChange(initial []byte, onChanged chan []byte) int {
	current := r.Current()
	if !bytes.Equal(current, initial) {
		go func() {
			onChanged <- current
		}()
	}
	return -1
}

func (r Reactive) CancelNotifyOnChange(i int) {
	return
}

type hybrid struct {
	current Reactive
	abort   Version
}

func (h hybrid) Current() []byte {
	return h.current.Current()
}

func (h hybrid) NotifyOnChange(initial []byte, onChanged chan []byte) int {
	// initial value will be from current, so discard it
	return h.abort.NotifyOnChange(h.abort.Current(), onChanged)
}

func (h hybrid) CancelNotifyOnChange(i int) {
	h.abort.CancelNotifyOnChange(i)
}

func CreateHybrid(current Reactive, abort Version) Version {
	return hybrid{current: current, abort: abort}
}

type listeners struct {
	current           []byte
	subscriptionIndex int
	subscriptionMutex *sync.Mutex
	subscriptions     map[int]chan []byte
}

func (l *listeners) Current() []byte {
	return l.current
}

func (l *listeners) NotifyOnChange(initial []byte, onChanged chan []byte) int {
	log.Debugf("setting up change notifier with %v as initial.", initial)
	l.subscriptionMutex.Lock()
	defer l.subscriptionMutex.Unlock()
	l.subscriptionIndex++
	l.subscriptions[l.subscriptionIndex] = onChanged
	if current := l.current; !bytes.Equal(current, initial) {
		go func() {
			onChanged <- current
		}()
	}
	log.Debugf("unsubscribe index is %v", l.subscriptionIndex)
	return l.subscriptionIndex
}

func (l *listeners) CancelNotifyOnChange(i int) {
	log.Debugln("cancelling notification on change")
	l.subscriptionMutex.Lock()
	defer l.subscriptionMutex.Unlock()
	close(l.subscriptions[i])
	delete(l.subscriptions, i)
}

func CreateListener(source chan []byte) *listeners {
	result := listeners{subscriptions: make(map[int]chan []byte), subscriptionMutex: new(sync.Mutex)}
	go func() {
		for {
			newValue := <-source
			if !bytes.Equal(newValue, result.current) {
				result.subscriptionMutex.Lock()
				log.Debugf("Notifying %v listeners that %v is now %v", len(result.subscriptions), string(result.current), string(newValue))
				result.current = newValue
				for _, listener := range result.subscriptions {
					select {
					case listener <- newValue:
					default:
					}
				}
				result.subscriptionMutex.Unlock()
			}
		}
	}()
	return &result
}

type (
	CacheableFunction func(stdout io.Writer, stderr io.Writer, abort chan []byte) (result []byte, err error)
)

func (c *cacher) Invalidate(h hash.Hash, v Version) error {
	if h == nil {
		return fmt.Errorf("No hasher was provided")
	}
	version := v.Current()
	cacheKey := h.Sum(version)
	cacheFile := filepath.Join(c.cacheDir, "/.cache-"+hex.EncodeToString(cacheKey))
	if err := os.Remove(cacheFile); err == nil {
		return err
	}
	return nil
}

/***
type Versioner interface {
	Sum() ([]byte, error)
}

type Ref interface {
	Save(hasher hash.Hash, versioner Versioner) error
	Load(hasher hash.Hash, versioner Versioner) (T, error)
}

func VersionedRef(marshalable interface{}, versioner func() ([]byte, error)) Ref {
	return nil
}
***/

type cacher struct {
	cacheDir string
}

// CacheableFunction func(stdout io.Writer, stderr io.Writer, abort chan []byte) (result []byte, err error)
type mockCacher struct{}

func (m *mockCacher) Cache(hasher hash.Hash, wrapped CacheableFunction, versioner Version) ([]byte, error) {
	return wrapped(os.Stdout, os.Stderr, make(chan []byte))
}

type Cacher interface {
	Cache(hasher hash.Hash, wrapped CacheableFunction, versioner Version) ([]byte, error)
}

func MakeCacher(name string) Cacher {
	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	cacheDir := filepath.Join(usr.HomeDir, ".cache", name)
	if !DirExists(cacheDir) {
		err := os.MkdirAll(cacheDir, 0700)
		if err != nil {
			log.Errorf("%v cannot be used as a cache directory, so caching is disabled.", cacheDir)
			return &mockCacher{}
		}
	}
	result := &cacher{cacheDir: cacheDir}
	go result.Cleanup()
	return result
}

func (c *cacher) Cleanup() {
	// delete cache files over a week old
	filepath.WalkDir(c.cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Debugf("Not listening for changes in %v: not readable", path)
			return nil
		}
		if fileInfo, err := os.Stat(path); err == nil {
			if fileInfo.ModTime().Add(time.Hour * 168).Before(time.Now()) {
				err = os.Remove(path)
				if err != nil {
					log.Infof("While trying to delete %v: %v", path, err)
				}
			}
		}
		return nil
	})
}

func (c *cacher) Cache(hasher hash.Hash, wrapped CacheableFunction, versioner Version) ([]byte, error) {
	if hasher == nil {
		return []byte(""), fmt.Errorf("No hasher was provided")
	}
	version := versioner.Current()
	if version == nil {
		// can't cache this, just run it "normally"
		log.Debugln("version is not available; not caching.")
		// no override of stderr/stdout; let them go to the normal place
		log.Debugln("About to run the wrapped command")
		return wrapped(os.Stdout, os.Stderr, nil)
	}
	cacheKey := hasher.Sum(version)
	hexCacheKey := hex.EncodeToString(cacheKey)
	cacheFile := filepath.Join(c.cacheDir, hexCacheKey+"-cache")
	stdoutFile := filepath.Join(c.cacheDir, hexCacheKey+"-stdout")
	stderrFile := filepath.Join(c.cacheDir, hexCacheKey+"-stderr")
	result, err := readBytes(cacheFile)
	if err == nil {
		stdout, err := readBytes(stdoutFile)
		if err == nil {
			stderr, err := readBytes(stderrFile)
			if err == nil {
				os.Stdout.Write(stdout)
				os.Stderr.Write(stderr)
				return result, nil
			}
		}
	}
	// couldn't read cached data for one reason or another
	log.Debugln(err)

	var stdout, stderr bytes.Buffer
	log.Debugln("About to run the wrapped command")
	stdoutMw := io.MultiWriter(&stdout, os.Stdout)
	stderrMw := io.MultiWriter(&stderr, os.Stderr)
	onChanged := make(chan []byte)
	defer versioner.CancelNotifyOnChange(versioner.NotifyOnChange(version, onChanged))
	result, resultError := wrapped(stdoutMw, stderrMw, onChanged)
	if resultError == nil {
		new_version := versioner.Current()
		if new_version == nil {
			log.Debugln("version is no longer available; not caching the result.")
			return result, err
		}
		if !bytes.Equal(new_version, version) {
			log.Infoln("version has changed during execution; not caching the result.")
			return result, err
		}
		if handle, err := os.Create(cacheFile); err == nil {
			if _, err := handle.Write(result); err == nil {
				if handle, err := os.Create(stdoutFile); err == nil {
					if _, err := handle.Write(stdout.Bytes()); err == nil {
						if handle, err := os.Create(stderrFile); err == nil {
							if _, err := handle.Write(stderr.Bytes()); err == nil {
							}
						}
					}
				}
			}
		}
	} else {
		log.Debugf("Not caching result as an error was returned: %v\n", resultError)
	}
	return result, resultError
}
