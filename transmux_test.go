package transmux

import (
	"testing"
	"runtime"
	"io"
	"fmt"
	"sync"
)

type pipehalf struct {
	other *pipehalf
	inqueue chan []byte
	leftovers []byte
	mtx sync.Mutex
	rmtx sync.Mutex
	closed bool
}

func (h *pipehalf) Read(p []byte) (n int, retErr error) {
	defer func() {
		fmt.Printf("Read(%v): %d, %v\n", p[:n], n, retErr)
	}()
	h.mtx.Lock()
	if h.closed {
		h.mtx.Unlock()
		return 0, io.EOF
	}
	h.mtx.Unlock()
	h.rmtx.Lock()
	defer h.rmtx.Unlock()
	if len(h.leftovers) > 0 {
		n := copy(p, h.leftovers)
		h.leftovers = h.leftovers[n:]
		return n, nil
	}
	d := <- h.inqueue
	n = copy(p, d)
	if len(d) > n {
		h.leftovers = d[n:]
	}
	return n, nil
}

func (h *pipehalf) Write(p []byte) (n int, retErr error) {
	defer func() {
		fmt.Printf("Write(%v): %d, %v\n", p, n, retErr)
	}()
	h.mtx.Lock()
	if h.closed {
		h.mtx.Unlock()
		return 0, io.EOF
	}
	h.mtx.Unlock()
	dup := make([]byte, len(p))
	copy(dup, p)
	h.other.inqueue <- dup
	return len(p), nil
}

func (h *pipehalf) Close() error {
	h.mtx.Lock()
	h.closed = true
	h.mtx.Unlock()
	h.other.mtx.Lock()
	h.other.closed = true
	h.other.mtx.Unlock()
	return nil
}

func pipe() (io.ReadWriteCloser, io.ReadWriteCloser) {
	l := &pipehalf{
		inqueue: make(chan []byte, 50),
	}
	r := &pipehalf{
		inqueue: make(chan []byte, 50),
	}
	l.other = r
	r.other = l
	return l, r
}

func TestPipe(t *testing.T) {
	l, r := pipe()
	l.Write([]byte{'Q'})
	buf := make([]byte, 2)
	n, err := r.Read(buf)
	if n != 1 || err != nil || buf[0] != 'Q' {
		t.Fatalf("rc1.Read(): %d, %v, %c; want: 2, nil, Q", n, err, buf[0])
	}
	r.Write([]byte{'X', 'Y', 'Z'})
	buf = make([]byte, 1)
	n, err = l.Read(buf)
	if n != 1 || err != nil || buf[0] != 'X' {
		t.Fatalf("rc1.Read(): %d, %v, %c; want: 2, nil, X", n, err, buf[0])
	}
	n, err = l.Read(buf)
	if n != 1 || err != nil || buf[0] != 'Y' {
		t.Fatalf("rc1.Read(): %d, %v, %c; want: 2, nil, Y", n, err, buf[0])
	}
	n, err = l.Read(buf)
	if n != 1 || err != nil || buf[0] != 'Z' {
		t.Fatalf("rc1.Read(): %d, %v, %c; want: 2, nil, Z", n, err, buf[0])
	}
}

func TestStreams(t *testing.T) {
	errch := make(chan error)
	r, w := pipe()
	go func() { errch <- func() error {
		var lastRChannel *Stream
		rt, err := WrapTransport(r, Unknown, func(s *Stream) {
			lastRChannel = s
		})
		if err != nil {
			return fmt.Errorf("WrapTransport(r): %v", err)
		}
		rc1, err := rt.NewStream()
		if err != nil {
			return fmt.Errorf("rt.NewStream(): %v", err)
		}
		for lastRChannel == nil {
			runtime.Gosched()
		}
		buf := make([]byte, 2)
		n, err := rc1.Read(buf)
		if n != 1 || err != nil {
			return fmt.Errorf("rc1.Read(): %d, %v; want: 2, nil", n, err)
		}
		n, err = lastRChannel.Read(buf)
		if n != 1 || err != nil {
			return fmt.Errorf("rc1.Read(): %d, %v; want: 2, nil", n, err)
		}
		return nil
	}() }()
	go func() { errch <- func() error {
		var lastWChannel *Stream
		wt, err := WrapTransport(w, Unknown, func(s *Stream) {
			lastWChannel = s
		})
		if err != nil {
			return fmt.Errorf("WrapTransport(w): %v", err)
		}
		wc1, err := wt.NewStream()
		if err != nil {
			return fmt.Errorf("wt.NewStream(): %v", err)
		}
		wc1.Write([]byte{'X'})
		for lastWChannel == nil {
			runtime.Gosched()
		}
		lastWChannel.Write([]byte{'Q'})
		return nil
	}() }()
	err := <-errch
	if err != nil {
		t.Fatal(err)
	}
	err = <-errch
	if err != nil {
		t.Fatal(err)
	}
}
