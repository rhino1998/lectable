// Command memtime runs a command and prints its peak RSS in KiB. Needed
// because a process exec'd straight from a large Go parent inherits the
// parent's RSS high-water mark in ru_maxrss.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func main() {
	c := exec.Command(os.Args[1], os.Args[2:]...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	err := c.Run()
	fmt.Fprintln(os.Stderr, "MAXRSS_KB", c.ProcessState.SysUsage().(*syscall.Rusage).Maxrss)
	if err != nil {
		os.Exit(1)
	}
}
