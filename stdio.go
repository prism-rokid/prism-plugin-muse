package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"

	sdk "github.com/Rokid-Prism/prism-plugin-sdk"
)

// The SDK reads and handles requests serially. Route long run waits to their
// own SDK dispatcher so the Hub can still read history while Muse is working.
func serve(ctx context.Context, adapter sdk.PluginAdapter, in io.Reader, out io.Writer) error {
	waitsCtx, cancel := context.WithCancel(ctx)
	var waits sync.WaitGroup
	output := &stdioOutput{writer: out}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	requests := &stdioRequests{
		scanner: scanner,
		dispatch: func(line []byte) {
			waits.Add(1)
			go func() {
				defer waits.Done()
				worker := sdk.StdioServer{Adapter: adapter, In: bytes.NewReader(line), Out: output}
				_ = worker.Run(waitsCtx)
			}()
		},
		cancelWaits: cancel,
	}
	server := sdk.StdioServer{Adapter: adapter, In: requests, Out: output}
	err := server.Run(ctx)
	cancel()
	waits.Wait()
	return err
}

type stdioRequests struct {
	scanner     *bufio.Scanner
	dispatch    func([]byte)
	cancelWaits context.CancelFunc
	pending     bytes.Reader
}

func (r *stdioRequests) Read(out []byte) (int, error) {
	for r.pending.Len() == 0 && r.scanner.Scan() {
		line := r.scanner.Bytes()
		var header struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(line, &header)
		switch header.Method {
		case "adapter.waitForRun":
			r.dispatch(bytes.Clone(line))
			continue
		case "adapter.close":
			r.cancelWaits()
		}
		r.pending.Reset(append(bytes.Clone(line), '\n'))
	}
	if err := r.scanner.Err(); err != nil {
		return 0, err
	}
	return r.pending.Read(out)
}

type stdioOutput struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *stdioOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}
