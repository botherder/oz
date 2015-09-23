package seccomp

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/subgraph/oz"
	"golang.org/x/sys/unix"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	STRINGARG = iota + 1
	PTRARG
	INTARG
)

type SystemCallArgs []int

func Tracer() {

	p := new(oz.Profile)
	if err := json.NewDecoder(os.Stdin).Decode(&p); err != nil {
		log.Error("unable to decode profile data: %v", err)
		os.Exit(1)
	}

	var proc_attr syscall.ProcAttr
	var sys_attr syscall.SysProcAttr

	sys_attr.Ptrace = true
	done := false
	proc_attr.Sys = &sys_attr

	cmd := os.Args[1]
	cmdArgs := os.Args[2:]
	log.Info("Tracer running command (%v) arguments (%v)\n", cmd, cmdArgs)
	c := exec.Command(cmd)
	c.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	c.Env = os.Environ()
	c.Args = append(c.Args, cmdArgs...)

	pi, err := c.StdinPipe()
	if err != nil {
		fmt.Errorf("error creating stdin pipe for tracer process: %v", err)
		os.Exit(1)
	}
	jdata, err := json.Marshal(p)
	if err != nil {
		fmt.Errorf("Unable to marshal seccomp state: %+v", err)
		os.Exit(1)
	}
	io.Copy(pi, bytes.NewBuffer(jdata))
	log.Info(string(jdata))
	pi.Close()

	children := make(map[int]bool)

	if err := c.Start(); err == nil {
		children[c.Process.Pid] = true
		var s syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &s, syscall.WALL, nil)
		children[pid] = true
		if err != nil {
			log.Error("Error (wait4) here first: %v %i", err, pid)
		}
		log.Info("Tracing child pid: %v\n", pid)
		for done == false {
			syscall.PtraceSetOptions(pid, unix.PTRACE_O_TRACESECCOMP|unix.PTRACE_O_TRACEFORK|unix.PTRACE_O_TRACEVFORK|unix.PTRACE_O_TRACECLONE)
			syscall.PtraceCont(pid, 0)
			pid, err = syscall.Wait4(-1, &s, syscall.WALL, nil)
			if err != nil {
				log.Error("Error (wait4) here: %v %i %v\n", err, pid, children)
				if len(children) == 0 {
					done = true
				}
				continue
			}
			children[pid] = true
			if s.Exited() == true {
				delete(children, pid)
				log.Info("Child pid %v finished.\n", pid)
				if len(children) == 0 {
					done = true
				}
				continue
			}
/*
		        if s.Signaled() == true {
			        log.Error("Other pid signalled %v %v", pid, s) 
				delete(children, pid)
																			                                        continue
																								                                }
*/
			switch uint32(s) >> 8 {

			case uint32(unix.SIGTRAP) | (unix.PTRACE_EVENT_SECCOMP << 8):
				if err != nil {
					log.Error("Error (ptrace): %v", err)
					continue
				}
				var regs syscall.PtraceRegs
				err = syscall.PtraceGetRegs(pid, &regs)

				if err != nil {
					log.Error("Error (ptrace): %v", err)
				}

				systemcall, err := syscallByNum(getSyscallNumber(regs))

				if err != nil {
					log.Error("Error: %v", err)
					continue
				}
				log.Info(renderSyscallBasic(pid, systemcall, regs))
				continue

			case uint32(unix.SIGTRAP) | (unix.PTRACE_EVENT_EXIT << 8):
				log.Error("Ptrace exit event detected pid %v (%s)", pid, getProcessCmdLine(pid))

			case uint32(unix.SIGTRAP) | (unix.PTRACE_EVENT_CLONE << 8):
				log.Error("Ptrace clone event detected pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			case uint32(unix.SIGTRAP) | (unix.PTRACE_EVENT_FORK << 8):
				log.Error("PTrace fork event detected pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			case uint32(unix.SIGTRAP) | (unix.PTRACE_EVENT_VFORK << 8):
				log.Error("Ptrace vfork event detected pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			case uint32(unix.SIGTRAP) | (unix.PTRACE_EVENT_VFORK_DONE << 8):
				log.Error("Ptrace vfork done event detected pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			case uint32(unix.SIGTRAP) | (unix.PTRACE_EVENT_EXEC << 8):
				log.Error("Ptrace exec event detected pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			case uint32(unix.SIGTRAP) | (unix.PTRACE_EVENT_STOP << 8):
				log.Error("Ptrace stop event detected pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			case uint32(unix.SIGTRAP):
				log.Error("SIGTRAP detected in pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			case uint32(unix.SIGCHLD):
				log.Error("SIGCHLD detected pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			case uint32(unix.SIGSTOP):
				log.Error("SIGSTOP detected pid %v (%s)", pid, getProcessCmdLine(pid))
				continue
			default:
				y := s.StopSignal()
				log.Error("Child stopped for unknown reasons pid %v status %v signal %i (%s)", pid, s, y, getProcessCmdLine(pid))
				continue
			}
		}
	}

}


func readStringArg(pid int, addr uintptr) (s string, err error) {
	buf := make([]byte, unsafe.Sizeof(addr))
	done := false
	err = nil
	for done == false {
		_, err := syscall.PtracePeekText(pid, addr, buf)
		if err != nil {
			fmt.Printf("Error (ptrace): %v\n", err)
		} else {
			for b := range buf {
				if buf[b] == 0 {
					done = true
					break
				} else {
					s += string(buf[b])
					if len(s) > 90 {
						s += "..."
						break
					}
				}
			}
		}
		addr += unsafe.Sizeof(addr)
	}
	return s, nil
}

func readUIntArg(pid int, addr uintptr) (uint64, error) {
	buf := make([]byte, unsafe.Sizeof(addr))

	_, err := syscall.PtracePeekText(pid, addr, buf)
	if err != nil {
		fmt.Printf("Error (ptrace): %v\n", err)
		return 0, err
	} else {
		i := binary.LittleEndian.Uint64(buf)
		return i, nil
	}
	return 0, errors.New("Error.")
}

func readPtrArg(pid int, addr uintptr) (uintptr, error) {

	buf := make([]byte, unsafe.Sizeof(addr))

	_, err := syscall.PtracePeekText(pid, addr, buf)
	if err != nil {
		fmt.Printf("Error (ptrace): %v\n", err)
		return 0, err
	} else {
		i := binary.LittleEndian.Uint64(buf)
		return uintptr(i), nil
	}
	return 0, nil
}

func syscallByNum(num int) (s SystemCall, err error) {
	var q SystemCall = SystemCall{"", "", -1, []int{0, 0, 0, 0, 0, 0}}
	for i := range syscalls {
		if syscalls[i].num == num {
			q = syscalls[i]
			return q, nil
		}
	}
	return q, errors.New("System call not found.\n")
}

func syscallByName(name string) (s SystemCall, err error) {
	var q SystemCall = SystemCall{"", "", -1, []int{0, 0, 0, 0, 0, 0}}
	for i := range syscalls {
		if syscalls[i].name == name {
			q = syscalls[i]
			return q, nil
		}
	}
	return q, errors.New("System call not found.\n")
}

func isPrintableASCII(s string) bool {
	for _, x := range s {
		if x > 127 {
			return false
		}
	}
	return true
}

func getProcessCmdLine(pid int) string {
	path := "/proc/" + strconv.Itoa(pid) + "/cmdline"
	cmdline, err := ioutil.ReadFile(path)
	for b := range cmdline {
		if b <= (len(cmdline) - 1) {
			if cmdline[b] == 0x00 {
				cmdline[b] = 0x20
			}
		}
	}
	if err != nil {
		log.Error("Error (read): %v", err)
		return "unknown"
	}
	return string(cmdline)
}
