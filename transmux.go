// Library transmux is a library that provides multiple streams over one stream.
package transmux

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"runtime"
	"sync"

	"github.com/Jille/dfr"
)

type Role int

const (
	Unknown Role = iota
	Master
	Slave
)

var StartReaderAutomatically = true

// post-handshake protocol:
// n[channel 1]: new channel
// c[channel 1]: close channel
// r[channel 1]: confirm channel close
// d[channel 1][size 2][data <65535]: data

type Transport struct {
	conn               io.ReadWriteCloser
	mtx                sync.Mutex
	writeMtx           sync.Mutex
	err                error
	nextChannel        uint32
	channels           map[uint32]*Stream
	newChannelCallback func(s *Stream)
	maxChunkSize       int
}

type Stream struct {
	t       *Transport
	id      uint32
	closed  bool
	inqueue [][]byte
	cv      *sync.Cond
}

var _ io.ReadWriteCloser = &Stream{}

func WrapTransport(conn io.ReadWriteCloser, role Role, cb func(s *Stream)) (*Transport, error) {
	// Do a handshake to fight over who gets the odd numbered channels.
	firstChannel := 1
	if role == Slave {
		firstChannel = 2
	}
	for role == Unknown {
		hs1 := []byte{'q', 's', 'c', '1', byte(rand.Intn(256))}
		hs2 := make([]byte, 5)
		_, err := conn.Write(hs1)
		if err != nil {
			return nil, err
		}
		_, err = io.ReadFull(conn, hs2)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(hs1[:4], hs2[:4]) {
			return nil, errors.New("protocol mismatch")
		}
		if hs1[4] != hs2[4] {
			if hs1[4] < hs2[4] {
				firstChannel = 2
				role = Slave
			}
			role = Master
			break
		}
	}
	t := &Transport{
		conn:               conn,
		nextChannel:        uint32(firstChannel),
		channels:           map[uint32]*Stream{},
		maxChunkSize:       4096,
		newChannelCallback: cb,
	}
	if StartReaderAutomatically {
		go t.Reader()
	}
	return t, nil
}

func (t *Transport) NewStream() (*Stream, error) {
	t.mtx.Lock()
	for _, exists := t.channels[t.nextChannel]; exists; {
		t.nextChannel += 2
	}
	s := &Stream{
		t:  t,
		id: t.nextChannel,
		cv: sync.NewCond(&t.mtx),
	}
	t.nextChannel += 2
	t.mtx.Unlock()
	buf := make([]byte, 5)
	buf[0] = 'n'
	binary.LittleEndian.PutUint32(buf[1:], s.id)
	err := t.write(buf)
	if err != nil {
		return nil, err
	}
	t.mtx.Lock()
	t.channels[s.id] = s
	t.mtx.Unlock()
	return s, nil
}

func (t *Transport) write(p []byte) error {
	t.writeMtx.Lock()
	defer t.writeMtx.Unlock()
	return t.writeLocked(p)
}

func (t *Transport) writeLocked(p []byte) error {
	if t.err != nil {
		return t.err
	}
	_, err := t.conn.Write(p)
	if err != nil {
		t.err = err
		return err
	}
	return nil
}

func (s *Stream) Close() error {
	d := dfr.D{}
	defer d.Run(nil)
	s.t.mtx.Lock()
	unlock := d.Add(s.t.mtx.Unlock)
	if s.t.err != nil {
		return s.t.err
	}
	if s.closed {
		return nil
	}
	s.closed = true
	unlock(true)

	buf := make([]byte, 5)
	buf[0] = 'd'
	binary.LittleEndian.PutUint32(buf[1:], s.id)
	return s.t.write(buf)
}

func (s *Stream) Write(p []byte) (n int, retErr error) {
	d := dfr.D{}
	defer d.Run(&retErr)
	buf := make([]byte, 7)
	buf[0] = 'd'
	binary.LittleEndian.PutUint32(buf[1:], s.id)
	n = 0
	for len(p) > 0 {
		s.t.mtx.Lock()
		unlock := d.Add(s.t.mtx.Unlock)
		if s.t.err != nil {
			return n, s.t.err
		}
		if s.closed {
			return n, io.EOF
		}
		unlock(true)
		l := s.t.maxChunkSize
		if len(p) < l {
			l = len(p)
		}
		binary.LittleEndian.PutUint16(buf[5:], uint16(l))
		s.t.writeMtx.Lock()
		unlock = d.Add(s.t.writeMtx.Unlock)
		err := s.t.writeLocked(buf)
		if err != nil {
			return n, err
		}
		err = s.t.writeLocked(p[:l])
		if err != nil {
			return n, err
		}
		unlock(true)
		p = p[l:]
		n += l
		// Allow other threads to interleave their data.
		runtime.Gosched()
	}
	return n, nil
}

func (s *Stream) Read(p []byte) (int, error) {
	s.t.mtx.Lock()
	defer s.t.mtx.Unlock()
	for len(s.inqueue) == 0 {
		if s.t.err != nil {
			return 0, s.t.err
		}
		if s.closed {
			return 0, io.EOF
		}
		s.cv.Wait()
	}
	n := copy(p, s.inqueue[0])
	if len(s.inqueue) > n {
		s.inqueue[0] = s.inqueue[0][n:]
	} else {
		if len(s.inqueue) > 1 {
			s.inqueue = s.inqueue[1:]
		} else {
			s.inqueue = nil
		}
	}
	return n, nil
}

func (t *Transport) Reader() error {
	if err := func() error {
		r := bufio.NewReader(t.conn)
		var hdr [5]byte
		for {
			_, err := io.ReadFull(r, hdr[:])
			if err != nil {
				return err
			}
			var s *Stream

			if err := func() error {
				t.mtx.Lock()
				defer t.mtx.Unlock()
				id := binary.LittleEndian.Uint32(hdr[1:5])
				switch hdr[0] {
				case 'n':
					s = &Stream{
						t:  t,
						id: id,
						cv: sync.NewCond(&t.mtx),
					}
					t.channels[id] = s
				case 'c':
					s = t.channels[id]
					s.closed = true
					go func() {
						// confirm the close and promise not to send any more data over it
						buf := make([]byte, 5)
						buf[0] = 'r'
						binary.LittleEndian.PutUint32(buf[1:], id)
						_ = t.write(buf)
					}()
				case 'r':
					delete(t.channels, id)
				case 'd':
					s = t.channels[id]
				default:
					return errors.New("protocol violation")
				}
				return nil
			}(); err != nil {
				return err
			}
			switch hdr[0] {
			case 'n':
				t.newChannelCallback(s)
			case 'c':
				s.cv.Broadcast()
			case 'd':
				_, err := io.ReadFull(r, hdr[:2])
				if err != nil {
					return err
				}
				l := binary.LittleEndian.Uint16(hdr[:2])
				b := make([]byte, l)
				_, err = io.ReadFull(r, b)
				if err != nil {
					return err
				}
				t.mtx.Lock()
				s.inqueue = append(s.inqueue, b)
				t.mtx.Unlock()
				s.cv.Signal()
			}
		}
	}(); err != nil {
		_ = t.conn.Close()
		t.mtx.Lock()
		if t.err == nil {
			t.err = err
		}
		for _, s := range t.channels {
			s.cv.Broadcast()
		}
		t.mtx.Unlock()
		return err
	}
	// TODO: unreachable?
	return nil
}

func (t *Transport) Close() error {
	// TODO: think
	return t.conn.Close()
}
