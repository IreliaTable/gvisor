// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package systrap

import (
	"fmt"
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/pool"
	"gvisor.dev/gvisor/pkg/seccomp"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/systrap/sysmsg"
	"gvisor.dev/gvisor/pkg/sentry/platform/systrap/usertrap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

var (
	// globalPool tracks all subprocesses in various state: active or available for
	// reuse.
	globalPool = subprocessPool{}

	// maximumUserAddress is the largest possible user address.
	maximumUserAddress = linux.TaskSize

	// stubInitAddress is the initial attempt link address for the stub.
	stubInitAddress = linux.TaskSize

	// maxRandomOffsetOfStubAddress is the maximum offset for randomizing a
	// stub address. It is set to the default value of mm.mmap_rnd_bits.
	//
	// Note: Tools like ThreadSanitizer don't like when the memory layout
	// is changed significantly.
	maxRandomOffsetOfStubAddress = (linux.TaskSize >> 7) & ^(uintptr(hostarch.PageSize) - 1)

	// maxStubUserAddress is the largest possible user address for
	// processes running inside gVisor. It is fixed because
	// * we don't want to reveal a stub address.
	// * it has to be the same across checkpoint/restore.
	maxStubUserAddress = maximumUserAddress - maxRandomOffsetOfStubAddress
)

// Linux kernel errnos which "should never be seen by user programs", but will
// be revealed to ptrace syscall exit tracing.
//
// These constants are only used in subprocess.go.
const (
	ERESTARTSYS    = unix.Errno(512)
	ERESTARTNOINTR = unix.Errno(513)
	ERESTARTNOHAND = unix.Errno(514)
)

// thread is a traced thread; it is a thread identifier.
//
// This is a convenience type for defining ptrace operations.
type thread struct {
	tgid int32
	tid  int32

	// sysmsgStackID is a stack ID in subprocess.sysmsgStackPool.
	sysmsgStackID uint64

	// initRegs are the initial registers for the first thread.
	//
	// These are used for the register set for system calls.
	initRegs arch.Registers
}

// requestThread is used to request a new sysmsg thread. A thread identifier will
// be sent into the thread channel.
type requestThread struct {
	thread chan *thread
}

// requestStub is used to request a new stub process.
type requestStub struct {
	done chan *thread
}

const (
	maxGuestThreads = 4096
)

// subprocess is a collection of threads being traced.
type subprocess struct {
	platform.NoAddressSpaceIO
	subprocessEntry

	// requests is used to signal creation of new threads.
	requests chan any

	// numContexts counts the number of contexts currently active within the
	// subprocess. A subprocess should not be fully released to be reused until
	// numContexts reaches 0.
	numContexts atomicbitops.Int32

	// mu protects the following fields.
	mu sync.Mutex

	// released marks this subprocess as having been released.
	// A subprocess can be both released and active because we cannot allow it to
	// reused until all tied contexts have been unregistered.
	released bool

	// contexts is the set of contexts for which it's possible that
	// context.lastFaultSP == this subprocess.
	contexts map[*context]struct{}

	// sysmsgStackPool is a pool of available sysmsg stacks.
	sysmsgStackPool pool.Pool

	// memoryFile is used to allocate a sysmsg stack which is shared
	// between a stub process and the Sentry.
	memoryFile *pgalloc.MemoryFile

	// usertrap is the state of the usertrap table which contains syscall
	// trampolines.
	usertrap *usertrap.State

	syscallThreadMu sync.Mutex
	syscallThread   *syscallThread
}

func (s *subprocess) initSyscallThread(ptraceThread *thread) error {
	s.syscallThreadMu.Lock()
	defer s.syscallThreadMu.Unlock()

	id, ok := s.sysmsgStackPool.Get()
	if !ok {
		panic("unable to allocate a sysmsg stub thread")
	}

	ptraceThread.sysmsgStackID = id
	t := syscallThread{
		subproc: s,
		thread:  ptraceThread,
	}

	if err := t.init(); err != nil {
		panic(fmt.Sprintf("failed to create a syscall thread"))
	}
	s.syscallThread = &t

	s.syscallThread.detach()

	return nil
}

// handlePtraceSyscallRequest executes system calls that can't be run via
// syscallThread without using ptrace. Look at the description of syscallThread
// to get more details about its limitations.
func (s *subprocess) handlePtraceSyscallRequest(req any) {
	s.syscallThreadMu.Lock()
	defer s.syscallThreadMu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	s.syscallThread.attach()
	defer s.syscallThread.detach()

	ptraceThread := s.syscallThread.thread

	switch req.(type) {
	case requestThread:
		r := req.(requestThread)
		t, err := ptraceThread.clone()
		if err != nil {
			// Should not happen: not recoverable.
			panic(fmt.Sprintf("error initializing first thread: %v", err))
		}

		// Since the new thread was created with
		// clone(CLONE_PTRACE), it will begin execution with
		// SIGSTOP pending and with this thread as its tracer.
		// (Hopefully nobody tgkilled it with a signal <
		// SIGSTOP before the SIGSTOP was delivered, in which
		// case that signal would be delivered before SIGSTOP.)
		if sig := t.wait(stopped); sig != unix.SIGSTOP {
			panic(fmt.Sprintf("error waiting for new clone: expected SIGSTOP, got %v", sig))
		}

		id, ok := s.sysmsgStackPool.Get()
		if !ok {
			panic("unable to allocate a sysmsg stub thread")
		}
		t.sysmsgStackID = id

		if _, _, e := unix.RawSyscall(unix.SYS_TGKILL, uintptr(t.tgid), uintptr(t.tid), uintptr(unix.SIGSTOP)); e != 0 {
			panic(fmt.Sprintf("tkill failed: %v", e))
		}

		// Detach the thread.
		t.detach()
		t.initRegs = ptraceThread.initRegs

		// Return the thread.
		r.thread <- t
	case requestStub:
		r := req.(requestStub)
		t, err := ptraceThread.createStub()
		if err != nil {
			panic(fmt.Sprintf("unable to create a stub process: %s", err))
		}
		r.done <- t

	}
}

// newSubprocess returns a usable subprocess.
//
// This will either be a newly created subprocess, or one from the global pool.
// The create function will be called in the latter case, which is guaranteed
// to happen with the runtime thread locked.
func newSubprocess(create func() (*thread, error), memoryFile *pgalloc.MemoryFile) (*subprocess, error) {
	if sp := globalPool.fetchAvailable(); sp != nil {
		return sp, nil
	}

	// The following goroutine is responsible for creating the first traced
	// thread, and responding to requests to make additional threads in the
	// traced process. The process will be killed and reaped when the
	// request channel is closed, which happens in Release below.
	requests := make(chan any)

	// Ready.
	sp := &subprocess{
		requests:        requests,
		contexts:        make(map[*context]struct{}),
		sysmsgStackPool: pool.Pool{Start: 0, Limit: maxGuestThreads},
		memoryFile:      memoryFile,
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Initialize the first thread.
	ptraceThread, err := create()
	if err != nil {
		return nil, err
	}

	if err := sp.initSyscallThread(ptraceThread); err != nil {
		return nil, err
	}

	go func() { // S/R-SAFE: Platform-related.

		// Wait for requests to create threads.
		for req := range requests {
			sp.handlePtraceSyscallRequest(req)
		}

		// Requests should never be closed.
		panic("unreachable")
	}()

	sp.unmap()
	sp.usertrap = usertrap.New()

	globalPool.add(sp)
	return sp, nil
}

// unmap unmaps non-stub regions of the process.
//
// This will panic on failure (which should never happen).
func (s *subprocess) unmap() {
	s.Unmap(0, uint64(stubStart))
	if maximumUserAddress != stubEnd {
		s.Unmap(hostarch.Addr(stubEnd), uint64(maximumUserAddress-stubEnd))
	}
}

// Release kills the subprocess.
//
// Just kidding! We can't safely co-ordinate the detaching of all the
// tracees (since the tracers are random runtime threads, and the process
// won't exit until tracers have been notifier).
//
// Therefore we simply unmap everything in the subprocess and return it to the
// globalPool. This has the added benefit of reducing creation time for new
// subprocesses.
func (s *subprocess) Release() {
	go func() { // S/R-SAFE: Platform.
		s.unmap()
		globalPool.release(s)
	}()
}

// newThread creates a new traced thread.
//
// Precondition: the OS thread must be locked.
func (s *subprocess) newThread() *thread {
	// Ask the first thread to create a new one.
	var r requestThread
	r.thread = make(chan *thread)
	s.requests <- r
	t := <-r.thread

	// Attach the subprocess to this one.
	t.attach()

	// Return the new thread, which is now bound.
	return t
}

// attach attaches to the thread.
func (t *thread) attach() {
	if _, _, errno := unix.RawSyscall6(unix.SYS_PTRACE, unix.PTRACE_ATTACH, uintptr(t.tid), 0, 0, 0, 0); errno != 0 {
		panic(fmt.Sprintf("unable to attach: %v", errno))
	}

	// PTRACE_ATTACH sends SIGSTOP, and wakes the tracee if it was already
	// stopped from the SIGSTOP queued by CLONE_PTRACE (see inner loop of
	// newSubprocess), so we always expect to see signal-delivery-stop with
	// SIGSTOP.
	if sig := t.wait(stopped); sig != unix.SIGSTOP {
		panic(fmt.Sprintf("wait failed: expected SIGSTOP, got %v", sig))
	}

	// Initialize options.
	t.init()
}

func (t *thread) grabInitRegs() {
	// Grab registers.
	//
	// Note that we adjust the current register RIP value to be just before
	// the current system call executed. This depends on the definition of
	// the stub itself.
	if err := t.getRegs(&t.initRegs); err != nil {
		panic(fmt.Sprintf("ptrace get regs failed: %v", err))
	}
	t.adjustInitRegsRip()
	t.initRegs.SetStackPointer(0)
}

// detach detaches from the thread.
//
// Because the SIGSTOP is not suppressed, the thread will enter group-stop.
func (t *thread) detach() {
	if _, _, errno := unix.RawSyscall6(unix.SYS_PTRACE, unix.PTRACE_DETACH, uintptr(t.tid), 0, uintptr(unix.SIGSTOP), 0, 0); errno != 0 {
		panic(fmt.Sprintf("can't detach new clone: %v", errno))
	}
}

// waitOutcome is used for wait below.
type waitOutcome int

const (
	// stopped indicates that the process was stopped.
	stopped waitOutcome = iota

	// killed indicates that the process was killed.
	killed
)

func (t *thread) Debugf(format string, v ...any) {
	prefix := fmt.Sprintf("%8d:", t.tid)
	log.DebugfAtDepth(1, prefix+format, v...)
}

func (t *thread) dumpAndPanic(message string) {
	var regs arch.Registers
	message += "\n"
	if err := t.getRegs(&regs); err == nil {
		message += dumpRegs(&regs)
	} else {
		log.Warningf("unable to get registers: %v", err)
	}
	message += fmt.Sprintf("stubStart\t = %016x\n", stubStart)
	panic(message)
}

func (t *thread) dumpRegs(message string) {
	var regs arch.Registers
	message += "\n"
	if err := t.getRegs(&regs); err == nil {
		message += dumpRegs(&regs)
	} else {
		log.Warningf("unable to get registers: %v", err)
	}
	log.Infof("%s", message)
}

func (t *thread) unexpectedStubExit() {
	msg, err := t.getEventMessage()
	status := unix.WaitStatus(msg)
	if status.Signaled() && status.Signal() == unix.SIGKILL {
		// SIGKILL can be only sent by a user or OOM-killer. In both
		// these cases, we don't need to panic. There is no reasons to
		// think that something wrong in gVisor.
		log.Warningf("The ptrace stub process %v has been killed by SIGKILL.", t.tgid)
		pid := os.Getpid()
		unix.Tgkill(pid, pid, unix.Signal(unix.SIGKILL))
	}
	t.dumpAndPanic(fmt.Sprintf("wait failed: the process %d:%d exited: %x (err %v)", t.tgid, t.tid, msg, err))
}

// wait waits for a stop event.
//
// Precondition: outcome is a valid waitOutcome.
func (t *thread) wait(outcome waitOutcome) unix.Signal {
	var status unix.WaitStatus

	for {
		r, err := unix.Wait4(int(t.tid), &status, unix.WALL|unix.WUNTRACED, nil)
		if err == unix.EINTR || err == unix.EAGAIN {
			// Wait was interrupted; wait again.
			continue
		} else if err != nil {
			panic(fmt.Sprintf("ptrace wait failed: %v", err))
		}
		if int(r) != int(t.tid) {
			panic(fmt.Sprintf("ptrace wait returned %v, expected %v", r, t.tid))
		}
		switch outcome {
		case stopped:
			if !status.Stopped() {
				t.dumpAndPanic(fmt.Sprintf("ptrace status unexpected: got %v, wanted stopped", status))
			}
			stopSig := status.StopSignal()
			if stopSig == 0 {
				continue // Spurious stop.
			}
			if stopSig == unix.SIGTRAP {
				if status.TrapCause() == unix.PTRACE_EVENT_EXIT {
					t.unexpectedStubExit()
				}
				// Re-encode the trap cause the way it's expected.
				return stopSig | unix.Signal(status.TrapCause()<<8)
			}
			// Not a trap signal.
			return stopSig
		case killed:
			if !status.Exited() && !status.Signaled() {
				t.dumpAndPanic(fmt.Sprintf("ptrace status unexpected: got %v, wanted exited", status))
			}
			return unix.Signal(status.ExitStatus())
		default:
			// Should not happen.
			t.dumpAndPanic(fmt.Sprintf("unknown outcome: %v", outcome))
		}
	}
}

// destroy kills the thread.
//
// Note that this should not be used in the general case; the death of threads
// will typically cause the death of the parent. This is a utility method for
// manually created threads.
func (t *thread) destroy() {
	t.detach()
	unix.Tgkill(int(t.tgid), int(t.tid), unix.Signal(unix.SIGKILL))
	t.wait(killed)
}

// init initializes trace options.
func (t *thread) init() {
	// Set the TRACESYSGOOD option to differentiate real SIGTRAP.
	// set PTRACE_O_EXITKILL to ensure that the unexpected exit of the
	// sentry will immediately kill the associated stubs.
	_, _, errno := unix.RawSyscall6(
		unix.SYS_PTRACE,
		unix.PTRACE_SETOPTIONS,
		uintptr(t.tid),
		0,
		unix.PTRACE_O_TRACESYSGOOD|unix.PTRACE_O_TRACEEXIT|unix.PTRACE_O_EXITKILL,
		0, 0)
	if errno != 0 {
		panic(fmt.Sprintf("ptrace set options failed: %v", errno))
	}
}

// syscall executes a system call cycle in the traced context.
//
// This is _not_ for use by application system calls, rather it is for use when
// a system call must be injected into the remote context (e.g. mmap, munmap).
// Note that clones are handled separately.
func (t *thread) syscall(regs *arch.Registers) (uintptr, error) {
	// Set registers.
	if err := t.setRegs(regs); err != nil {
		panic(fmt.Sprintf("ptrace set regs failed: %v", err))
	}

	for {
		// Execute the syscall instruction. The task has to stop on the
		// trap instruction which is right after the syscall
		// instruction.
		if _, _, errno := unix.RawSyscall6(unix.SYS_PTRACE, unix.PTRACE_CONT, uintptr(t.tid), 0, 0, 0, 0); errno != 0 {
			panic(fmt.Sprintf("ptrace syscall-enter failed: %v", errno))
		}

		sig := t.wait(stopped)
		if sig == unix.SIGTRAP {
			// Reached syscall-enter-stop.
			break
		} else {
			// Some other signal caused a thread stop; ignore.
			if sig != unix.SIGSTOP && sig != unix.SIGCHLD {
				log.Warningf("The thread %d:%d has been interrupted by %d", t.tgid, t.tid, sig)
			}
			continue
		}
	}

	// Grab registers.
	if err := t.getRegs(regs); err != nil {
		panic(fmt.Sprintf("ptrace get regs failed: %v", err))
	}
	return syscallReturnValue(regs)
}

// syscallIgnoreInterrupt ignores interrupts on the system call thread and
// restarts the syscall if the kernel indicates that should happen.
func (t *thread) syscallIgnoreInterrupt(
	initRegs *arch.Registers,
	sysno uintptr,
	args ...arch.SyscallArgument) (uintptr, error) {
	for {
		regs := createSyscallRegs(initRegs, sysno, args...)
		rval, err := t.syscall(&regs)
		switch err {
		case ERESTARTSYS:
			continue
		case ERESTARTNOINTR:
			continue
		case ERESTARTNOHAND:
			continue
		default:
			return rval, err
		}
	}
}

// NotifyInterrupt implements interrupt.Receiver.NotifyInterrupt.
func (t *thread) NotifyInterrupt() {
	unix.Tgkill(int(t.tgid), int(t.tid), unix.Signal(platform.SignalInterrupt))
}

// switchToApp is called from the main SwitchToApp entrypoint.
//
// This function returns true on a system call, false on a signal.
// The second return value is true if a syscall instruction can be replaced on
// a function call.
func (s *subprocess) switchToApp(c *context, ac *arch.Context64) (isSyscall bool, shouldPatchSyscall bool, err error) {
	// Reset necessary registers.
	regs := &ac.StateData().Regs
	sysThread, err := s.getSysmsgThread(regs, c, ac)
	if err != nil {
		return false, false, err
	}
	msg := sysThread.msg
	t := sysThread.thread
	t.resetSysemuRegs(regs)

	s.restoreFPState(msg, sysThread.fpuStateToMsgOffset, c, ac)

	// Check for interrupts, and ensure that future interrupts will signal t.
	if !c.interrupt.Enable(sysThread) {
		// Pending interrupt; simulate.
		c.signalInfo = linux.SignalInfo{Signo: int32(platform.SignalInterrupt)}
		return false, false, nil
	}
	defer c.interrupt.Disable()

	restoreArchSpecificState(regs, t, sysThread, msg, ac)
	msg.Regs = regs.PtraceRegs
	msg.EnableSentryFastPath()
	sysThread.waitEvent(sysmsg.StateDone)

	if msg.Type != sysmsg.EventTypeSyscallTrap {
		var err error
		sysThread.fpuStateToMsgOffset, err = msg.FPUStateOffset()
		if err != nil {
			return false, false, err
		}
	} else {
		sysThread.fpuStateToMsgOffset = 0
	}

	if msg.Err != 0 {
		panic(fmt.Sprintf("stub thread %d failed: err %d line %d: %s", t.tid, msg.Err, msg.Line, msg))
	}

	regs.PtraceRegs = msg.Regs
	retrieveArchSpecificState(regs, msg, t, ac)

	// We have a signal. We verify however, that the signal was
	// either delivered from the kernel or from this process. We
	// don't respect other signals.
	c.signalInfo = msg.SignalInfo
	if msg.Type == sysmsg.EventTypeSyscallCanBePatched {
		msg.Type = sysmsg.EventTypeSyscall
		shouldPatchSyscall = true
	}

	if msg.Type == sysmsg.EventTypeSyscall || msg.Type == sysmsg.EventTypeSyscallTrap {
		if maybePatchSignalInfo(regs, &c.signalInfo) {
			return false, false, nil
		}
		updateSyscallRegs(regs)
		return true, shouldPatchSyscall, nil
	} else if msg.Type != sysmsg.EventTypeFault {
		panic(fmt.Sprintf("unknown message type: %v", msg.Type))
	}

	return false, false, nil
}

// syscall executes the given system call without handling interruptions.
func (s *subprocess) syscall(sysno uintptr, args ...arch.SyscallArgument) (uintptr, error) {
	s.syscallThreadMu.Lock()
	defer s.syscallThreadMu.Unlock()

	return s.syscallThread.syscall(sysno, args...)
}

// MapFile implements platform.AddressSpace.MapFile.
func (s *subprocess) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	var flags int
	if precommit {
		flags |= unix.MAP_POPULATE
	}
	_, err := s.syscall(
		unix.SYS_MMAP,
		arch.SyscallArgument{Value: uintptr(addr)},
		arch.SyscallArgument{Value: uintptr(fr.Length())},
		arch.SyscallArgument{Value: uintptr(at.Prot())},
		arch.SyscallArgument{Value: uintptr(flags | unix.MAP_SHARED | unix.MAP_FIXED)},
		arch.SyscallArgument{Value: uintptr(f.FD())},
		arch.SyscallArgument{Value: uintptr(fr.Start)})
	return err
}

// Unmap implements platform.AddressSpace.Unmap.
func (s *subprocess) Unmap(addr hostarch.Addr, length uint64) {
	ar, ok := addr.ToRange(length)
	if !ok {
		panic(fmt.Sprintf("addr %#x + length %#x overflows", addr, length))
	}
	s.mu.Lock()
	for c := range s.contexts {
		c.mu.Lock()
		if c.lastFaultSP == s && ar.Contains(c.lastFaultAddr) {
			// Forget the last fault so that if c faults again, the fault isn't
			// incorrectly reported as a write fault. If this is being called
			// due to munmap() of the corresponding vma, handling of the second
			// fault will fail anyway.
			c.lastFaultSP = nil
			delete(s.contexts, c)
		}
		c.mu.Unlock()
	}
	s.mu.Unlock()
	_, err := s.syscall(
		unix.SYS_MUNMAP,
		arch.SyscallArgument{Value: uintptr(addr)},
		arch.SyscallArgument{Value: uintptr(length)})
	if err != nil {
		// We never expect this to happen.
		panic(fmt.Sprintf("munmap(%x, %x)) failed: %v", addr, length, err))
	}
}

// getSysmsgThread returns a sysmsg thread for the specified context.
func (s *subprocess) getSysmsgThread(tregs *arch.Registers, c *context, ac *arch.Context64) (*sysmsgThread, error) {
	sysThread := c.sysmsgThread
	if sysThread != nil && sysThread.subproc != s {
		// This can happen if a new address space
		// has been created (e.g. fork).
		sysThread.destroy()
		sysThread = nil
	}
	if sysThread != nil {
		return sysThread, nil
	}

	// Create a new seccomp process.
	var r requestThread
	r.thread = make(chan *thread)
	s.requests <- r
	p := <-r.thread

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p.attach()

	// Skip SIGSTOP.
	if _, _, errno := unix.RawSyscall6(unix.SYS_PTRACE, unix.PTRACE_CONT, uintptr(p.tid), 0, 0, 0, 0); errno != 0 {
		panic(fmt.Sprintf("ptrace cont failed: %v", errno))
	}
	sig := p.wait(stopped)
	if sig != unix.SIGSTOP {
		panic(fmt.Sprintf("error waiting for new clone: expected SIGSTOP, got %v", sig))
	}

	// Allocate a new stack for the BPF process.
	opts := pgalloc.AllocOpts{
		Kind: usage.System,
		Dir:  pgalloc.TopDown,
	}
	fr, err := s.memoryFile.Allocate(uint64(sysmsg.PerThreadSharedStackSize), opts)
	if err != nil {
		// TODO(b/144063246): Need to fail the clone system call.
		panic(fmt.Sprintf("failed to allocate a new stack: %v", err))
	}
	sysThread = &sysmsgThread{
		thread:     p,
		subproc:    s,
		stackRange: fr,
	}

	// Map the stack into the sentry.
	sentryStackAddr, _, errno := unix.RawSyscall6(
		unix.SYS_MMAP,
		0,
		sysmsg.PerThreadSharedStackSize,
		unix.PROT_WRITE|unix.PROT_READ,
		unix.MAP_SHARED|unix.MAP_FILE,
		uintptr(s.memoryFile.FD()), uintptr(fr.Start))
	if errno != 0 {
		panic(fmt.Sprintf("mmap failed: %v", errno))
	}

	// Before installing the stub syscall filters, we need to call a few
	// system calls (e.g. sigaltstack, sigaction) which have in-memory
	// arguments.  We need to prevent changing these parameters by other
	// stub threads, so lets map the future BPF stack as read-only and
	// fill syscall arguments from the Sentry.
	sysmsgStackAddr := sysThread.sysmsgPerThreadMemAddr() + sysmsg.PerThreadSharedStackOffset
	err = sysThread.mapStack(sysmsgStackAddr, true)
	if err != nil {
		panic(fmt.Sprintf("mmap failed: %v", err))
	}

	sysThread.init(sentryStackAddr, sysmsgStackAddr)

	// Map the stack into the BPF process.
	err = sysThread.mapStack(sysmsgStackAddr, false)
	if err != nil {
		s.memoryFile.DecRef(fr)
		panic(fmt.Sprintf("mmap failed: %v", err))
	}

	// Map the stack into the BPF process.
	privateStackAddr := sysThread.sysmsgPerThreadMemAddr() + sysmsg.PerThreadPrivateStackOffset
	err = sysThread.mapPrivateStack(privateStackAddr, sysmsg.PerThreadPrivateStackSize)
	if err != nil {
		s.memoryFile.DecRef(fr)
		panic(fmt.Sprintf("mmap failed: %v", err))
	}

	sysThread.setMsg(sysmsg.StackAddrToMsg(sentryStackAddr))
	sysThread.msg.Init()
	sysThread.msg.Self = uint64(sysmsgStackAddr + sysmsg.MsgOffsetFromSharedStack)
	sysThread.msg.Syshandler = uint64(stubSysmsgStart + uintptr(sysmsg.Sighandler_blob_offset____export_syshandler))
	sysThread.msg.SyshandlerStack = uint64(sysmsg.StackAddrToSyshandlerStack(sysThread.sysmsgPerThreadMemAddr()))

	sysThread.msg.State.Set(sysmsg.StateDone)

	// Install a pre-compiled seccomp rules for the BPF process.
	_, err = p.syscallIgnoreInterrupt(&p.initRegs, unix.SYS_PRCTL,
		arch.SyscallArgument{Value: uintptr(linux.PR_SET_NO_NEW_PRIVS)},
		arch.SyscallArgument{Value: uintptr(1)},
		arch.SyscallArgument{Value: uintptr(0)})
	if err != nil {
		panic(fmt.Sprintf("prctl(PR_SET_NO_NEW_PRIVS) failed: %v", err))
	}

	_, err = p.syscallIgnoreInterrupt(&p.initRegs, seccomp.SYS_SECCOMP,
		arch.SyscallArgument{Value: uintptr(linux.SECCOMP_SET_MODE_FILTER)},
		arch.SyscallArgument{Value: uintptr(0)},
		arch.SyscallArgument{Value: stubSysmsgRules})
	if err != nil {
		panic(fmt.Sprintf("seccomp failed: %v", err))
	}

	// Prepare to start the BPF process.
	p.resetSysemuRegs(tregs)
	archSpecificSysThreadInit(sysThread, tregs)
	if err := p.setRegs(tregs); err != nil {
		panic(fmt.Sprintf("ptrace set regs failed: %v", err))
	}
	// Send a fake event to stop the BPF process.
	if _, _, e := unix.RawSyscall(unix.SYS_TGKILL, uintptr(p.tgid), uintptr(p.tid), uintptr(unix.SIGSEGV)); e != 0 {
		panic(fmt.Sprintf("tkill failed: %v", e))
	}
	// Skip SIGSTOP.
	if _, _, e := unix.RawSyscall(unix.SYS_TGKILL, uintptr(p.tgid), uintptr(p.tid), uintptr(unix.SIGCONT)); e != 0 {
		panic(fmt.Sprintf("tkill failed: %v", e))
	}
	// Resume the BPF process.
	if _, _, errno := unix.RawSyscall6(unix.SYS_PTRACE, unix.PTRACE_DETACH, uintptr(p.tid), 0, 0, 0, 0); errno != 0 {
		panic(fmt.Sprintf("can't detach new clone: %v", errno))
	}

	sysThread.waitEvent(sysmsg.StateNone)
	if msg := sysThread.msg; msg.Err != 0 {
		panic(fmt.Sprintf("stub thread failed: %v (line %v)", msg.Err, msg.Line))
	}

	sysThread.fpuStateToMsgOffset, err = sysThread.msg.FPUStateOffset()
	if err != nil {
		sysThread.destroy()
		return nil, err
	}

	c.sysmsgThread = sysThread

	return sysThread, nil
}

// PreFork implements platform.AddressSpace.PreFork.
// We need to take the usertrap lock to be sure that fork() will not be in the
// middle of applying a binary patch.
func (s *subprocess) PreFork() {
	s.usertrap.PreFork()
}

// PostFork implements platform.AddressSpace.PostFork.
func (s *subprocess) PostFork() {
	s.usertrap.PostFork() // +checklocksforce: PreFork acquires, above.
}

// unregisterContext releases all references held for this context.
//
// Precondition: context c must have been active within subprocess s.
func (s *subprocess) unregisterContext(c *context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.contexts, c)
	s.numContexts.Add(-1)
	released := s.released
	s.mu.Unlock()

	if released && s.numContexts.Load() == 0 {
		globalPool.release(s)
	}
}
