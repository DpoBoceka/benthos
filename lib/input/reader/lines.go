// Copyright (c) 2018 Ashley Jeffs
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package reader

import (
	"bufio"
	"bytes"
	"io"
	"time"

	"github.com/Jeffail/benthos/v3/lib/message"
	"github.com/Jeffail/benthos/v3/lib/types"
)

//------------------------------------------------------------------------------

// Lines is a reader implementation that continuously reads line delimited
// messages from an io.Reader type.
type Lines struct {
	handleCtor func() (io.Reader, error)
	onClose    func()

	handle  io.Reader
	scanner *bufio.Scanner

	messageBuffer      *bytes.Buffer
	messageBufferIndex int

	maxBuffer int
	multipart bool
	delimiter []byte
}

// NewLines creates a new reader input type.
//
// Callers must provide a constructor function for the target io.Reader, which
// is called on start up and again each time a reader is exhausted. If the
// constructor is called but there is no more content to create a Reader for
// then the error `io.EOF` should be returned and the Lines will close.
//
// Callers must also provide an onClose function, which will be called if the
// Lines has been instructed to shut down. This function should unblock any
// blocked Read calls.
func NewLines(
	handleCtor func() (io.Reader, error),
	onClose func(),
	options ...func(r *Lines),
) (*Lines, error) {
	r := Lines{
		handleCtor:    handleCtor,
		onClose:       onClose,
		messageBuffer: &bytes.Buffer{},
		maxBuffer:     bufio.MaxScanTokenSize,
		multipart:     false,
		delimiter:     []byte("\n"),
	}

	for _, opt := range options {
		opt(&r)
	}

	return &r, nil
}

//------------------------------------------------------------------------------

// OptLinesSetMaxBuffer is a option func that sets the maximum size of the
// line parsing buffers.
func OptLinesSetMaxBuffer(maxBuffer int) func(r *Lines) {
	return func(r *Lines) {
		r.maxBuffer = maxBuffer
	}
}

// OptLinesSetMultipart is a option func that sets the boolean flag
// indicating whether lines should be parsed as multipart or not.
func OptLinesSetMultipart(multipart bool) func(r *Lines) {
	return func(r *Lines) {
		r.multipart = multipart
	}
}

// OptLinesSetDelimiter is a option func that sets the delimiter (default
// '\n') used to divide lines (message parts) in the stream of data.
func OptLinesSetDelimiter(delimiter string) func(r *Lines) {
	return func(r *Lines) {
		r.delimiter = []byte(delimiter)
	}
}

//------------------------------------------------------------------------------

func (r *Lines) closeHandle() {
	if r.handle != nil {
		if closer, ok := r.handle.(io.ReadCloser); ok {
			closer.Close()
		}
		r.handle = nil
	}
	r.scanner = nil
}

// Connect attempts to establish a new scanner for an io.Reader.
func (r *Lines) Connect() error {
	if r.scanner != nil {
		return nil
	}
	r.closeHandle() // Just incase we have an open handle without a scanner.

	var err error
	r.handle, err = r.handleCtor()
	if err != nil {
		if err == io.EOF {
			return types.ErrTypeClosed
		}
		return err
	}

	r.scanner = bufio.NewScanner(r.handle)
	if r.maxBuffer != bufio.MaxScanTokenSize {
		r.scanner.Buffer([]byte{}, r.maxBuffer)
	}

	r.scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}

		if i := bytes.Index(data, r.delimiter); i >= 0 {
			// We have a full terminated line.
			return i + len(r.delimiter), data[0:i], nil
		}

		// If we're at EOF, we have a final, non-terminated line. Return it.
		if atEOF {
			return len(data), data, nil
		}

		// Request more data.
		return 0, nil, nil
	})

	return nil
}

// Read attempts to read a new line from the io.Reader.
func (r *Lines) Read() (types.Message, error) {
	if r.scanner == nil {
		return nil, types.ErrNotConnected
	}

	msg := message.New(nil)

	for r.scanner.Scan() {
		partSize, err := r.messageBuffer.Write(r.scanner.Bytes())
		rIndex := r.messageBufferIndex
		r.messageBufferIndex += partSize
		if err != nil {
			return nil, err
		}

		// WARNING: According to https://golang.org/pkg/bytes/#Buffer.Bytes the
		// slice returned by Bytes is only correct until the next call to Write.
		// Since we call Write for multiple part messages, and could potentially
		// call it on a consecutive Read call before the next Acknowledge, we
		// are passing slices through messages that are "invalid".
		//
		// However, in practice the calls to Write do not overwrite the memory
		// within the returned slice even if it results in the buffer
		// re-allocating memory. Since we also ensure that Reset is only called
		// once messages are no longer in use we should be fine here.
		//
		// Regardless, we should regularly revisit this code in order to ensure
		// that this remains the case. I can't foresee any case where discarded
		// slices within bytes.Buffer would be wiped or modified during Write,
		// but since the library does not guarantee this:
		//
		// TODO: Have another cheeky gander at
		// https://golang.org/src/bytes/buffer.go to make sure Write never
		// mutates a discarded slice during re-allocation. If it does then we
		// should stop using bytes.Buffer and either eat the allocations or do
		// some buffer rotations of our own.
		if partSize > 0 {
			msg.Append(message.NewPart(r.messageBuffer.Bytes()[rIndex : rIndex+partSize : rIndex+partSize]))
			if !r.multipart {
				return msg, nil
			}
		} else if r.multipart && msg.Len() > 0 {
			// Empty line means we're finished reading parts for this
			// message.
			return msg, nil
		}
	}

	if err := r.scanner.Err(); err != nil {
		r.closeHandle()
		return nil, err
	}

	r.closeHandle()

	if msg.Len() > 0 {
		return msg, nil
	}
	return nil, types.ErrNotConnected
}

// Acknowledge confirms whether or not our unacknowledged messages have been
// successfully propagated or not.
func (r *Lines) Acknowledge(err error) error {
	if err == nil && r.messageBuffer != nil {
		r.messageBuffer.Reset()
		r.messageBufferIndex = 0
	}
	return nil
}

// CloseAsync shuts down the reader input and stops processing requests.
func (r *Lines) CloseAsync() {
	r.onClose()
}

// WaitForClose blocks until the reader input has closed down.
func (r *Lines) WaitForClose(timeout time.Duration) error {
	r.closeHandle()
	return nil
}

//------------------------------------------------------------------------------
