package main

import "time"

func deadlineAfter(millis int) func() bool {
	deadline := time.Now().Add(time.Duration(millis) * time.Millisecond)
	return func() bool {
		return time.Now().After(deadline)
	}
}

func sleep5ms() {
	time.Sleep(5 * time.Millisecond)
}
