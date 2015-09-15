package proc

import (
	"fmt"
	"runtime"
)

// Represents a single breakpoint. Stores information on the break
// point including the byte of data that originally was stored at that
// address.
type Breakpoint struct {
	// File & line information for printing.
	FunctionName string
	File         string
	Line         int

	Addr         uint64 // Address breakpoint is set for.
	OriginalData []byte // If software breakpoint, the data we replace with breakpoint instruction.
	ID           int    // Monotonically increasing ID.
	Temp         bool   // Whether this is a temp breakpoint (for next'ing).

	// Breakpoint information
	Tracepoint bool     // Tracepoint flag
	Stacktrace int      // Number of stack frames to retrieve
	Goroutine  bool     // Retrieve goroutine information
	Variables  []string // Variables to evaluate

	hardware bool // Breakpoint using CPU debug registers.
	reg      int  // If hardware breakpoint, what debug register it belongs to.

}

func (bp *Breakpoint) String() string {
	return fmt.Sprintf("Breakpoint %d at %#v %s:%d", bp.ID, bp.Addr, bp.File, bp.Line)
}

// Clear this breakpoint appropriately depending on whether it is a
// hardware or software breakpoint.
func (bp *Breakpoint) Clear(thread *Thread) (*Breakpoint, error) {
	if bp.hardware {
		if err := thread.dbp.clearHardwareBreakpoint(bp.reg, thread.Id); err != nil {
			return nil, err
		}
		return bp, nil
	}
	if _, err := thread.writeMemory(uintptr(bp.Addr), bp.OriginalData); err != nil {
		return nil, fmt.Errorf("could not clear breakpoint %s", err)
	}
	return bp, nil
}

// Returned when trying to set a breakpoint at
// an address that already has a breakpoint set for it.
type BreakpointExistsError struct {
	file string
	line int
	addr uint64
}

func (bpe BreakpointExistsError) Error() string {
	return fmt.Sprintf("Breakpoint exists at %s:%d at %x", bpe.file, bpe.line, bpe.addr)
}

// InvalidAddressError represents the result of
// attempting to set a breakpoint at an invalid address.
type InvalidAddressError struct {
	address uint64
}

func (iae InvalidAddressError) Error() string {
	return fmt.Sprintf("Invalid address %#v\n", iae.address)
}

func (dbp *Process) setBreakpoint(tid int, addr uint64, temp bool) (*Breakpoint, error) {
	if bp, ok := dbp.FindBreakpoint(addr); ok {
		return nil, BreakpointExistsError{bp.File, bp.Line, bp.Addr}
	}

	f, l, fn := dbp.goSymTable.PCToLine(uint64(addr))
	if fn == nil {
		return nil, InvalidAddressError{address: addr}
	}

	newBreakpoint := &Breakpoint{
		FunctionName: fn.Name,
		File:         f,
		Line:         l,
		Addr:         addr,
		Temp:         temp,
	}

	if temp {
		dbp.tempBreakpointIDCounter++
		newBreakpoint.ID = dbp.tempBreakpointIDCounter
	} else {
		dbp.breakpointIDCounter++
		newBreakpoint.ID = dbp.breakpointIDCounter
	}

	// Try and set a hardware breakpoint.
	for i, used := range dbp.arch.HardwareBreakpointUsage() {
		if runtime.GOOS == "darwin" { // TODO(dp): Implement hardware breakpoints on OSX.
			break
		}
		if used {
			continue
		}
		for tid, t := range dbp.Threads {
			if t.running {
				err := t.Halt()
				if err != nil {
					return nil, err
				}
				defer t.Continue()
			}
			if err := dbp.setHardwareBreakpoint(i, tid, addr); err != nil {
				return nil, fmt.Errorf("could not set hardware breakpoint on thread %d: %s", t, err)
			}
		}
		dbp.arch.SetHardwareBreakpointUsage(i, true)
		newBreakpoint.reg = i
		newBreakpoint.hardware = true
		dbp.Breakpoints[addr] = newBreakpoint
		return dbp.Breakpoints[addr], nil
	}

	// Fall back to software breakpoint.
	thread := dbp.Threads[tid]
	originalData, err := thread.readMemory(uintptr(addr), dbp.arch.BreakpointSize())
	if err != nil {
		return nil, err
	}
	if err := dbp.writeSoftwareBreakpoint(thread, addr); err != nil {
		return nil, err
	}
	newBreakpoint.OriginalData = originalData
	dbp.Breakpoints[addr] = newBreakpoint

	return newBreakpoint, nil
}

func (dbp *Process) writeSoftwareBreakpoint(thread *Thread, addr uint64) error {
	_, err := thread.writeMemory(uintptr(addr), dbp.arch.BreakpointInstruction())
	return err
}

// Error thrown when trying to clear a breakpoint that does not exist.
type NoBreakpointError struct {
	addr uint64
}

func (nbp NoBreakpointError) Error() string {
	return fmt.Sprintf("no breakpoint at %#v", nbp.addr)
}
