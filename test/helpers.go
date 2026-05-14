package test

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"testing"
)

// runCmd runs a bash command, streaming stdout+stderr into the test log line
// by line, and returning the process and the first error from Start/Wait.
// envs are appended to the inherited os.Environ.
func runCmd(t *testing.T, cmd, workDir string, envs []string, callback func(*exec.Cmd)) (*exec.Cmd, error) {
	t.Logf("running command: %s", cmd)
	c := exec.Command("bash", "-c", cmd)
	c.Env = append(os.Environ(), envs...)
	c.Dir = workDir
	if callback != nil {
		callback(c)
	}
	stdout, _ := c.StdoutPipe()
	stderr, _ := c.StderrPipe()
	if err := c.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(io.MultiReader(stderr, stdout))
		for scanner.Scan() {
			t.Log(scanner.Text())
		}
	}()
	<-done
	if err := c.Wait(); err != nil {
		return nil, err
	}
	return c, nil
}
