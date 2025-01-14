package datagramsession

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// TestCloseSession makes sure a session will stop after context is done
func TestSessionCtxDone(t *testing.T) {
	testSessionReturns(t, closeByContext, time.Minute*2)
}

// TestCloseSession makes sure a session will stop after close method is called
func TestCloseSession(t *testing.T) {
	testSessionReturns(t, closeByCallingClose, time.Minute*2)
}

// TestCloseIdle makess sure a session will stop after there is no read/write for a period defined by closeAfterIdle
func TestCloseIdle(t *testing.T) {
	testSessionReturns(t, closeByTimeout, time.Millisecond*100)
}

func testSessionReturns(t *testing.T, closeBy closeMethod, closeAfterIdle time.Duration) {
	sessionID := uuid.New()
	cfdConn, originConn := net.Pipe()
	payload := testPayload(sessionID)
	transport := &mockQUICTransport{
		reqChan:  newDatagramChannel(1),
		respChan: newDatagramChannel(1),
	}
	session := newSession(sessionID, transport, cfdConn)

	ctx, cancel := context.WithCancel(context.Background())
	sessionDone := make(chan struct{})
	go func() {
		session.Serve(ctx, closeAfterIdle)
		close(sessionDone)
	}()

	go func() {
		n, err := session.transportToDst(payload)
		require.NoError(t, err)
		require.Equal(t, len(payload), n)
	}()

	readBuffer := make([]byte, len(payload)+1)
	n, err := originConn.Read(readBuffer)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)

	lastRead := time.Now()

	switch closeBy {
	case closeByContext:
		cancel()
	case closeByCallingClose:
		session.close()
	}

	<-sessionDone
	if closeBy == closeByTimeout {
		require.True(t, time.Now().After(lastRead.Add(closeAfterIdle)))
	}
	// call cancelled again otherwise the linter will warn about possible context leak
	cancel()
}

type closeMethod int

const (
	closeByContext closeMethod = iota
	closeByCallingClose
	closeByTimeout
)

func TestWriteToDstSessionPreventClosed(t *testing.T) {
	testActiveSessionNotClosed(t, false, true)
}

func TestReadFromDstSessionPreventClosed(t *testing.T) {
	testActiveSessionNotClosed(t, true, false)
}

func testActiveSessionNotClosed(t *testing.T, readFromDst bool, writeToDst bool) {
	const closeAfterIdle = time.Millisecond * 100
	const activeTime = time.Millisecond * 500

	sessionID := uuid.New()
	cfdConn, originConn := net.Pipe()
	payload := testPayload(sessionID)
	transport := &mockQUICTransport{
		reqChan:  newDatagramChannel(100),
		respChan: newDatagramChannel(100),
	}
	session := newSession(sessionID, transport, cfdConn)

	startTime := time.Now()
	activeUntil := startTime.Add(activeTime)
	ctx, cancel := context.WithCancel(context.Background())
	errGroup, ctx := errgroup.WithContext(ctx)
	errGroup.Go(func() error {
		session.Serve(ctx, closeAfterIdle)
		if time.Now().Before(startTime.Add(activeTime)) {
			return fmt.Errorf("session closed while it's still active")
		}
		return nil
	})

	if readFromDst {
		errGroup.Go(func() error {
			for {
				if time.Now().After(activeUntil) {
					return nil
				}
				if _, err := originConn.Write(payload); err != nil {
					return err
				}
				time.Sleep(closeAfterIdle / 2)
			}
		})
	}
	if writeToDst {
		errGroup.Go(func() error {
			readBuffer := make([]byte, len(payload))
			for {
				n, err := originConn.Read(readBuffer)
				if err != nil {
					if err == io.EOF || err == io.ErrClosedPipe {
						return nil
					}
					return err
				}
				if !bytes.Equal(payload, readBuffer[:n]) {
					return fmt.Errorf("payload %v is not equal to %v", readBuffer[:n], payload)
				}
			}
		})
		errGroup.Go(func() error {
			for {
				if time.Now().After(activeUntil) {
					return nil
				}
				if _, err := session.transportToDst(payload); err != nil {
					return err
				}
				time.Sleep(closeAfterIdle / 2)
			}
		})
	}

	require.NoError(t, errGroup.Wait())
	cancel()
}

func TestMarkActiveNotBlocking(t *testing.T) {
	const concurrentCalls = 50
	session := newSession(uuid.New(), nil, nil)
	var wg sync.WaitGroup
	wg.Add(concurrentCalls)
	for i := 0; i < concurrentCalls; i++ {
		go func() {
			session.markActive()
			wg.Done()
		}()
	}
	wg.Wait()
}
