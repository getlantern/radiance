package client

import (
	"context"
	"fmt"
	"time"

	"github.com/sagernet/sing-box/experimental/libbox"
)

func SendCmd(cmd int32) (*CmdClientHandler, error) {
	handler := newCmdClientHandler()
	opts := libbox.CommandClientOptions{Command: cmd, StatusInterval: int64(time.Second)}
	cc := libbox.NewCommandClient(handler, &opts)
	if err := cc.Connect(); err != nil {
		return nil, fmt.Errorf("connecting to command client: %w", err)
	}
	defer cc.Disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	select {
	case <-handler.done:
		return handler, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type CmdClientHandler struct {
	Status      *libbox.StatusMessage
	Connections *libbox.Connections
	ClashMode   string
	Groups      []*libbox.OutboundGroup
	connected   chan struct{}
	done        chan struct{}
}

func newCmdClientHandler() *CmdClientHandler {
	return &CmdClientHandler{
		connected: make(chan struct{}, 1),
		done:      make(chan struct{}, 1),
	}
}

func (c *CmdClientHandler) Connected() {
	c.connected <- struct{}{}
}
func (c *CmdClientHandler) Disconnected(message string) {}
func (c *CmdClientHandler) WriteStatus(message *libbox.StatusMessage) {
	c.Status = message
	c.done <- struct{}{}
}
func (c *CmdClientHandler) InitializeClashMode(modeList libbox.StringIterator, currentMode string) {
	c.ClashMode = currentMode
	c.done <- struct{}{}
}
func (c *CmdClientHandler) UpdateClashMode(newMode string) {
	c.ClashMode = newMode
	c.done <- struct{}{}
}
func (c *CmdClientHandler) WriteConnections(message *libbox.Connections) {
	c.Connections = message
	c.done <- struct{}{}
}

func (c *CmdClientHandler) WriteGroups(message libbox.OutboundGroupIterator) {
	groups := message
	for groups.HasNext() {
		c.Groups = append(c.Groups, groups.Next())
	}
	c.done <- struct{}{}
}

// Not Implemented
func (c *CmdClientHandler) ClearLogs()                                  { c.done <- struct{}{} }
func (c *CmdClientHandler) WriteLogs(messageList libbox.StringIterator) { c.done <- struct{}{} }
