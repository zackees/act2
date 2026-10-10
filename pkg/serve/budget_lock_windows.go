package serve

import (
	"errors"
	"os"
)

// tryLock: the cache budget runs only inside a Linux engine.
func tryLock(*os.File) (func(), error) {
	return nil, errors.New("the cache budget needs a Unix host")
}
