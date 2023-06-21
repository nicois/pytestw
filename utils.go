package main

// panics if there is an error
func Assert[T any](t T, e error) T {
	if e != nil {
		panic(e)
	}
	return t
}
