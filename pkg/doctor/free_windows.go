package doctor

import "errors"

func freeBytes(string) (uint64, error) { return 0, errors.New("not measured on Windows") }
