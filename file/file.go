package file

import (
	"fmt"
	"os"

	set "github.com/nicois/pytestw/set"
	log "github.com/sirupsen/logrus"
)

type Paths = set.StringSet

type (
	Void struct{}
)

func CreatePaths(paths ...string) Paths {
	return Paths(set.Create(paths...))
}

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

func ReadBytes(filename string) ([]byte, error) {
	if handle, err := os.Open(filename); err == nil {
		defer handle.Close()
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
