package client

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/containers/conmon-rs/internal/proto"
	"github.com/containers/podman/v3/libpod/define"
	"github.com/containers/podman/v3/pkg/kubeutils"
	"github.com/containers/podman/v3/utils"
	"github.com/pkg/errors"
)

const (
	attachPacketBufSize = 8192
	attachPipeStdin     = 1
	attachPipeStdout    = 2
	attachPipeStderr    = 3
)

type AttachStreams struct {
	Stdin                                   io.Reader
	Stdout, Stderr                          io.WriteCloser
	AttachStdin, AttachStdout, AttachStderr bool
}

type AttachConfig struct {
	// ID of the container.
	ID string
	// Path of the attach socket.
	SocketPath string
	// ExecSession ID, if this is an attach for an Exec.
	ExecSession string
	// Whether a terminal was setup for the command this is attaching to.
	Tty bool
	// Whether stdout/stderr should continue to be processed after stdin is closed.
	StopAfterStdinEOF bool
	// Whether the output is passed through the caller's std streams, rather than
	// ones created for the attach session.
	Passthrough bool
	// Channel of resize events.
	Resize chan define.TerminalSize
	// The standard streams for this attach session.
	Streams AttachStreams
	// A closure to be run before the streams are attached.
	// This could be used to start a container.
	PreAttachFunc func() error
	// A closure to be run after the streams are attached.
	// This could be used to notify callers the streams have been attached.
	PostAttachFunc func() error
	// The keys that indicate the attach session should be detached.
	DetachKeys []byte
}

func (c *ConmonClient) AttachContainer(ctx context.Context, cfg *AttachConfig) error {
	conn, err := c.newRPCConn()
	if err != nil {
		return err
	}
	defer conn.Close()

	client := proto.Conmon{Client: conn.Bootstrap(ctx)}
	future, free := client.AttachContainer(ctx, func(p proto.Conmon_attachContainer_Params) error {
		req, err := p.NewRequest()
		if err != nil {
			return err
		}
		if err := req.SetId(cfg.ID); err != nil {
			return err
		}
		if err := req.SetSocketPath(cfg.SocketPath); err != nil {
			return err
		}
		// TODO: add exec session
		return nil
	})
	defer free()

	result, err := future.Struct()
	if err != nil {
		return err
	}

	if _, err := result.Response(); err != nil {
		return err
	}

	return c.attach(ctx, cfg)
}

func (c *ConmonClient) attach(ctx context.Context, cfg *AttachConfig) error {
	var (
		conn *net.UnixConn
		err  error
	)
	if !cfg.Passthrough {
		c.logger.Debugf("Attaching to container %s", cfg.ID)

		kubeutils.HandleResizing(cfg.Resize, func(size define.TerminalSize) {
			c.logger.Debugf("Got a resize event: %+v", size)
			if err := c.SetWindowSizeContainer(ctx, &SetWindowSizeContainerConfig{
				ID:   cfg.ID,
				Size: &size,
			}); err != nil {
				c.logger.Debugf("Failed to write to control file to resize terminal: %v", err)
			}
		})

		conn, err = DialLongSocket("unixpacket", cfg.SocketPath)
		if err != nil {
			return errors.Wrapf(err, "failed to connect to container's attach socket: %v", cfg.SocketPath)
		}
		defer func() {
			if err := conn.Close(); err != nil {
				c.logger.Errorf("unable to close socket: %q", err)
			}
		}()
	}

	if cfg.PreAttachFunc != nil {
		if err := cfg.PreAttachFunc(); err != nil {
			return err
		}
	}

	if cfg.Passthrough {
		return nil
	}

	receiveStdoutError, stdinDone := c.setupStdioChannels(cfg, conn)
	if cfg.PostAttachFunc != nil {
		if err := cfg.PostAttachFunc(); err != nil {
			return err
		}
	}

	return c.readStdio(cfg, conn, receiveStdoutError, stdinDone)
}
func (c *ConmonClient) setupStdioChannels(cfg *AttachConfig, conn *net.UnixConn) (chan error, chan error) {
	receiveStdoutError := make(chan error)
	go func() {
		receiveStdoutError <- c.redirectResponseToOutputStreams(cfg, conn)
	}()

	stdinDone := make(chan error)
	go func() {
		var err error
		if cfg.Streams.AttachStdin {
			_, err = utils.CopyDetachable(conn, cfg.Streams.Stdin, cfg.DetachKeys)
		}
		stdinDone <- err
	}()

	return receiveStdoutError, stdinDone
}

func (c *ConmonClient) redirectResponseToOutputStreams(cfg *AttachConfig, conn io.Reader) (err error) {
	buf := make([]byte, attachPacketBufSize+1) /* Sync with conmonrs ATTACH_PACKET_BUF_SIZE */
	for {
		nr, er := conn.Read(buf)
		if nr > 0 {
			var dst io.Writer
			var doWrite bool
			switch buf[0] {
			case attachPipeStdout:
				dst = cfg.Streams.Stdout
				doWrite = cfg.Streams.AttachStdout
			case attachPipeStderr:
				dst = cfg.Streams.Stderr
				doWrite = cfg.Streams.AttachStderr
			default:
				c.logger.Infof("Received unexpected attach type %+d", buf[0])
			}
			if dst == nil {
				return errors.New("output destination cannot be nil")
			}

			if doWrite {
				nw, ew := dst.Write(buf[1:nr])
				if ew != nil {
					err = ew
					break
				}
				if nr != nw+1 {
					err = io.ErrShortWrite
					break
				}
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			err = er
			break
		}
	}
	return err
}

func (c *ConmonClient) readStdio(cfg *AttachConfig, conn *net.UnixConn, receiveStdoutError, stdinDone chan error) error {
	var err error
	select {
	case err = <-receiveStdoutError:
		conn.CloseWrite()
		return err
	case err = <-stdinDone:
		// This particular case is for when we get a non-tty attach
		// with --leave-stdin-open=true. We want to return as soon
		// as we receive EOF from the client. However, we should do
		// this only when stdin is enabled. If there is no stdin
		// enabled then we wait for output as usual.
		if cfg.StopAfterStdinEOF {
			return nil
		}
		if err == define.ErrDetach {
			conn.CloseWrite()
			return err
		}
		if err == nil {
			// copy stdin is done, close it
			if connErr := conn.CloseWrite(); connErr != nil {
				c.logger.Errorf("Unable to close conn: %v", connErr)
			}
		}
		if cfg.Streams.AttachStdout || cfg.Streams.AttachStderr {
			return <-receiveStdoutError
		}
	}
	return nil
}

type SetWindowSizeContainerConfig struct {
	ID   string
	Size *define.TerminalSize
}

func (c *ConmonClient) SetWindowSizeContainer(ctx context.Context, cfg *SetWindowSizeContainerConfig) error {
	if cfg.Size == nil {
		return fmt.Errorf("Terminal size cannot be nil")
	}

	conn, err := c.newRPCConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	client := proto.Conmon{Client: conn.Bootstrap(ctx)}

	future, free := client.SetWindowSizeContainer(ctx, func(p proto.Conmon_setWindowSizeContainer_Params) error {
		req, err := p.NewRequest()
		if err != nil {
			return err
		}
		if err := req.SetId(cfg.ID); err != nil {
			return err
		}
		req.SetWidth(cfg.Size.Width)
		req.SetHeight(cfg.Size.Height)
		return p.SetRequest(req)
	})
	defer free()

	result, err := future.Struct()
	if err != nil {
		return err
	}

	if _, err := result.Response(); err != nil {
		return err
	}

	return nil
}
