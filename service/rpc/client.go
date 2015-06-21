package rpc

import (
	"fmt"
	"log"
	"net/rpc"
	"net/rpc/jsonrpc"

	"github.com/derekparker/delve/service"
	"github.com/derekparker/delve/service/api"
)

// Client is a RPC service.Client.
type RPCClient struct {
	addr   string
	client *rpc.Client
}

// Ensure the implementation satisfies the interface.
var _ service.Client = &RPCClient{}

// NewClient creates a new RPCClient.
func NewClient(addr string) *RPCClient {
	client, err := jsonrpc.Dial("tcp", addr)
	if err != nil {
		log.Fatal("dialing:", err)
	}
	return &RPCClient{
		addr:   addr,
		client: client,
	}
}

func (c *RPCClient) Detach(kill bool) error {
	return c.call("Detach", kill, nil)
}

func (c *RPCClient) GetState() (*api.DebuggerState, error) {
	state := new(api.DebuggerState)
	err := c.call("State", nil, state)
	return state, err
}

func (c *RPCClient) Continue() (*api.DebuggerState, error) {
	state := new(api.DebuggerState)
	err := c.call("Command", &api.DebuggerCommand{Name: api.Continue}, state)
	return state, err
}

func (c *RPCClient) Next() (*api.DebuggerState, error) {
	state := new(api.DebuggerState)
	err := c.call("Command", &api.DebuggerCommand{Name: api.Next}, state)
	return state, err
}

func (c *RPCClient) Step() (*api.DebuggerState, error) {
	state := new(api.DebuggerState)
	err := c.call("Command", &api.DebuggerCommand{Name: api.Step}, state)
	return state, err
}

func (c *RPCClient) SwitchThread(threadID int) (*api.DebuggerState, error) {
	state := new(api.DebuggerState)
	cmd := &api.DebuggerCommand{
		Name:     api.SwitchThread,
		ThreadID: threadID,
	}
	err := c.call("Command", cmd, state)
	return state, err
}

func (c *RPCClient) Halt() (*api.DebuggerState, error) {
	state := new(api.DebuggerState)
	err := c.call("Command", &api.DebuggerCommand{Name: api.Halt}, state)
	return state, err
}

func (c *RPCClient) GetBreakpoint(id int) (*api.Breakpoint, error) {
	breakpoint := new(api.Breakpoint)
	err := c.call("GetBreakpoint", id, breakpoint)
	return breakpoint, err
}

func (c *RPCClient) CreateBreakpoint(breakPoint *api.Breakpoint) (*api.Breakpoint, error) {
	newBreakpoint := new(api.Breakpoint)
	err := c.call("CreateBreakpoint", breakPoint, &newBreakpoint)
	return newBreakpoint, err
}

func (c *RPCClient) ListBreakpoints() ([]*api.Breakpoint, error) {
	var breakpoints []*api.Breakpoint
	err := c.call("ListBreakpoints", nil, &breakpoints)
	return breakpoints, err
}

func (c *RPCClient) ClearBreakpoint(id int) (*api.Breakpoint, error) {
	bp := new(api.Breakpoint)
	err := c.call("ClearBreakpoint", id, bp)
	return bp, err
}

func (c *RPCClient) ListThreads() ([]*api.Thread, error) {
	var threads []*api.Thread
	err := c.call("ListThreads", nil, &threads)
	return threads, err
}

func (c *RPCClient) GetThread(id int) (*api.Thread, error) {
	thread := new(api.Thread)
	err := c.call("GetThread", id, &thread)
	return thread, err
}

func (c *RPCClient) EvalVariable(symbol string) (*api.Variable, error) {
	v := new(api.Variable)
	err := c.call("EvalSymbol", symbol, v)
	return v, err
}

func (c *RPCClient) EvalVariableFor(threadID int, symbol string) (*api.Variable, error) {
	v := new(api.Variable)
	err := c.call("EvalThreadSymbol", threadID, v)
	return v, err
}

func (c *RPCClient) ListSources(filter string) ([]string, error) {
	var sources []string
	err := c.call("ListSources", filter, &sources)
	return sources, err
}

func (c *RPCClient) ListFunctions(filter string) ([]string, error) {
	var funcs []string
	err := c.call("ListFunctions", filter, &funcs)
	return funcs, err
}

func (c *RPCClient) ListPackageVariables(filter string) ([]api.Variable, error) {
	var vars []api.Variable
	err := c.call("ListPackageVars", filter, &vars)
	return vars, err
}

func (c *RPCClient) ListPackageVariablesFor(threadID int, filter string) ([]api.Variable, error) {
	var vars []api.Variable
	err := c.call("ListThreadPackageVars", &ThreadListArgs{Id: threadID, Filter: filter}, &vars)
	return vars, err
}

func (c *RPCClient) ListLocalVariables() ([]api.Variable, error) {
	var vars []api.Variable
	err := c.call("ListLocalVars", nil, &vars)
	return vars, err
}

func (c *RPCClient) ListRegisters() (string, error) {
	var regs string
	err := c.call("ListRegisters", nil, &regs)
	return regs, err
}

func (c *RPCClient) ListFunctionArgs() ([]api.Variable, error) {
	var vars []api.Variable
	err := c.call("ListFunctionArgs", nil, &vars)
	return vars, err
}

func (c *RPCClient) ListGoroutines() ([]*api.Goroutine, error) {
	var goroutines []*api.Goroutine
	err := c.call("ListGoroutines", nil, &goroutines)
	return goroutines, err
}

func (c *RPCClient) Stacktrace(goroutineId, depth int) ([]*api.Location, error) {
	var locations []*api.Location
	err := c.call("StacktraceGoroutine", &StacktraceGoroutineArgs{Id: 1, Depth: depth}, &locations)
	return locations, err
}

func (c *RPCClient) url(path string) string {
	return fmt.Sprintf("http://%s%s", c.addr, path)
}

func (c *RPCClient) call(method string, args, reply interface{}) error {
	return c.client.Call("RPCServer."+method, args, reply)
}
