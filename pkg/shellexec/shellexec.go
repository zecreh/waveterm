// Copyright 2024, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package shellexec

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/wavetermdev/thenextwave/pkg/util/shellutil"
	"github.com/wavetermdev/thenextwave/pkg/wavebase"
)

type TermSize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

type CommandOptsType struct {
	Interactive bool              `json:"interactive,omitempty"`
	Login       bool              `json:"login,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

type ShellProc struct {
	Cmd       *exec.Cmd
	Pty       *os.File
	CloseOnce *sync.Once
	DoneCh    chan any // closed after proc.Wait() returns
	WaitErr   error    // WaitErr is synchronized by DoneCh (written before DoneCh is closed) and CloseOnce
}

func (sp *ShellProc) Close() {
	sp.Cmd.Process.Kill()
	go func() {
		_, waitErr := sp.Cmd.Process.Wait()
		sp.SetWaitErrorAndSignalDone(waitErr)
		sp.Pty.Close()
	}()
}

func (sp *ShellProc) SetWaitErrorAndSignalDone(waitErr error) {
	sp.CloseOnce.Do(func() {
		sp.WaitErr = waitErr
		close(sp.DoneCh)
	})
}

func (sp *ShellProc) Wait() error {
	<-sp.DoneCh
	return sp.WaitErr
}

// returns (done, waitError)
func (sp *ShellProc) WaitNB() (bool, error) {
	select {
	case <-sp.DoneCh:
		return true, sp.WaitErr
	default:
		return false, nil
	}
}

func ExitCodeFromWaitErr(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.ExitStatus()
		}
	}
	return -1

}

func setBoolConditionally(rval reflect.Value, field string, value bool) {
	if rval.Elem().FieldByName(field).IsValid() {
		rval.Elem().FieldByName(field).SetBool(value)
	}
}

func setSysProcAttrs(cmd *exec.Cmd) {
	rval := reflect.ValueOf(cmd.SysProcAttr)
	setBoolConditionally(rval, "Setsid", true)
	setBoolConditionally(rval, "Setctty", true)
}

func checkCwd(cwd string) error {
	if cwd == "" {
		return fmt.Errorf("cwd is empty")
	}
	if _, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("error statting cwd %q: %w", cwd, err)
	}
	return nil
}

func StartShellProc(termSize TermSize, cmdStr string, cmdOpts CommandOptsType) (*ShellProc, error) {
	var ecmd *exec.Cmd
	var shellOpts []string
	if cmdOpts.Login {
		shellOpts = append(shellOpts, "-l")
	}
	if cmdOpts.Interactive {
		shellOpts = append(shellOpts, "-i")
	}
	if cmdStr == "" {
		shellPath := shellutil.DetectLocalShellPath()
		ecmd = exec.Command(shellPath, shellOpts...)
	} else {
		shellPath := shellutil.DetectLocalShellPath()
		shellOpts = append(shellOpts, "-c", cmdStr)
		ecmd = exec.Command(shellPath, shellOpts...)
	}
	ecmd.Env = os.Environ()
	if cmdOpts.Cwd != "" {
		ecmd.Dir = cmdOpts.Cwd
	}
	if cwdErr := checkCwd(ecmd.Dir); cwdErr != nil {
		ecmd.Dir = wavebase.GetHomeDir()
	}
	envToAdd := shellutil.WaveshellEnvVars(shellutil.DefaultTermType)
	if os.Getenv("LANG") == "" {
		envToAdd["LANG"] = wavebase.DetermineLang()
	}
	shellutil.UpdateCmdEnv(ecmd, envToAdd)
	cmdPty, cmdTty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("opening new pty: %w", err)
	}
	if termSize.Rows == 0 || termSize.Cols == 0 {
		termSize.Rows = shellutil.DefaultTermRows
		termSize.Cols = shellutil.DefaultTermCols
	}
	if termSize.Rows <= 0 || termSize.Cols <= 0 {
		return nil, fmt.Errorf("invalid term size: %v", termSize)
	}
	pty.Setsize(cmdPty, &pty.Winsize{Rows: uint16(termSize.Rows), Cols: uint16(termSize.Cols)})
	ecmd.Stdin = cmdTty
	ecmd.Stdout = cmdTty
	ecmd.Stderr = cmdTty
	ecmd.SysProcAttr = &syscall.SysProcAttr{}
	setSysProcAttrs(ecmd)
	err = ecmd.Start()
	cmdTty.Close()
	if err != nil {
		cmdPty.Close()
		return nil, err
	}
	return &ShellProc{Cmd: ecmd, Pty: cmdPty, CloseOnce: &sync.Once{}, DoneCh: make(chan any)}, nil
}

func RunSimpleCmdInPty(ecmd *exec.Cmd, termSize TermSize) ([]byte, error) {
	ecmd.Env = os.Environ()
	shellutil.UpdateCmdEnv(ecmd, shellutil.WaveshellEnvVars(shellutil.DefaultTermType))
	cmdPty, cmdTty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("opening new pty: %w", err)
	}
	if termSize.Rows == 0 || termSize.Cols == 0 {
		termSize.Rows = shellutil.DefaultTermRows
		termSize.Cols = shellutil.DefaultTermCols
	}
	if termSize.Rows <= 0 || termSize.Cols <= 0 {
		return nil, fmt.Errorf("invalid term size: %v", termSize)
	}
	pty.Setsize(cmdPty, &pty.Winsize{Rows: uint16(termSize.Rows), Cols: uint16(termSize.Cols)})
	ecmd.Stdin = cmdTty
	ecmd.Stdout = cmdTty
	ecmd.Stderr = cmdTty
	ecmd.SysProcAttr = &syscall.SysProcAttr{}
	setSysProcAttrs(ecmd)
	err = ecmd.Start()
	cmdTty.Close()
	if err != nil {
		cmdPty.Close()
		return nil, err
	}
	defer cmdPty.Close()
	ioDone := make(chan bool)
	var outputBuf bytes.Buffer
	go func() {
		// ignore error (/dev/ptmx has read error when process is done)
		defer close(ioDone)
		io.Copy(&outputBuf, cmdPty)
	}()
	exitErr := ecmd.Wait()
	if exitErr != nil {
		return nil, exitErr
	}
	<-ioDone
	return outputBuf.Bytes(), nil
}
