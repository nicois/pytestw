package main

import (
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
)

type (
	Void  struct{}
	Paths map[string]Void
)

func CreatePaths(paths ...string) Paths {
	result := make(Paths)
	for _, path := range paths {
		result[path] = Member
	}
	return result
}

func (p *Paths) Discard(other Paths) {
	for o := range other {
		delete(*p, o)
	}
}

func (p *Paths) Union(other Paths) {
	for o := range other {
		(*p)[o] = Member
	}
}

var Member Void

func PathExists(filename string) bool {
	_, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return true
}

func DirExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func readBytes(filename string) ([]byte, error) {
	if handle, err := os.Open(filename); err == nil {
		stat, err := handle.Stat()
		if err != nil {
			log.Debugln("Could not stat() the cache file")
			return []byte{}, err
		}
		result := make([]byte, stat.Size())
		bytes_read, err := handle.Read(result)
		if err != nil {
			log.Debugln("Could not Read() the cache file")
			return []byte{}, err
		}
		if int64(bytes_read) != stat.Size() {
			return []byte{}, fmt.Errorf("Did not read expected number of bytes: %v", bytes_read)
		}
		log.Debugln("Using the cached result.")
		return result, nil
	} else {
		return []byte{}, fmt.Errorf("File %v could not be opened, maybe as it doesn't exist: %v", filename, err)
	}
}
