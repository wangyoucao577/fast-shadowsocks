package pipe

import (
	"container/list"
	"encoding/binary"
	"errors"
	"github.com/dearplain/fast-shadowsocks/ikcp"
	"net"
	"sync"
	"time"
	//"log"

	//"runtime"
	//"io/ioutil"
	//"os"
)

const dataLimit = 4000

const (
	Reset     byte = 0
	FirstSYN  byte = 6
	FirstACK  byte = 1
	SndSYN    byte = 2
	SndACK    byte = 2
	Data      byte = 4
	Ping      byte = 5
	Close     byte = 7
	CloseBack byte = 8
	ResetAck  byte = 9
)

func udp_output(buf []byte, _len int32, kcp *ikcp.Ikcpcb, user interface{}) int32 {
	c := (user).(*UDPContext)
	c.sock.WriteToUDP(buf[:_len], c.remote)
	return 0
}

type UDPContext struct {
	sock   *net.UDPConn
	remote *net.UDPAddr
}

type UDPMakeSession struct {
	UDPContext
	l        *Listener
	kcp      *ikcp.Ikcpcb
	id       uint32
	waitData chan bool
	sync.RWMutex
	close bool

	readCache       *list.List
	readCacheLen    int
	readCacheReaded int

	lastRead  time.Time
	lastWrite time.Time

	quitChan chan bool
}

type Listener struct {
	sock       *net.UDPConn
	sessions   map[string]*UDPMakeSession
	sessionIDs map[uint32]*UDPMakeSession
	id         uint32

	sync.RWMutex

	close bool

	newSession chan bool

	waitQueue     *list.List
	waitQueueLock sync.RWMutex

	quitChan chan bool
}

type Action struct {
	c   chan bool
	arg interface{}
}

// func SavePanic() {

// r := recover()
// if r == nil {
// return
// }

// buf := make([]byte, 10000)
// buf = buf[:runtime.Stack(buf, false)]

// ioutil.WriteFile("err.txt", buf, 0644)
// os.Exit(1)
// }

func newListener(local string) (*Listener, error) {

	l := &Listener{
		sessions:   make(map[string]*UDPMakeSession),
		sessionIDs: make(map[uint32]*UDPMakeSession),
		RWMutex:    sync.RWMutex{}, newSession: make(chan bool, 1),
		waitQueue: list.New(), waitQueueLock: sync.RWMutex{},
		quitChan: make(chan bool)}

	if local != "" {

		udpAddr, err := net.ResolveUDPAddr("udp", local)
		if err != nil {
			return nil, err
		}

		sock, _err := net.ListenUDP("udp", udpAddr)
		if _err != nil {
			return nil, _err
		}

		l.sock = sock

	} else {

		sock, _err := net.ListenUDP("udp", &net.UDPAddr{})
		if _err != nil {
			return nil, _err
		}

		l.sock = sock

	}

	go l.recvLoop()

	return l, nil
}

func (l *Listener) newID() uint32 {
	id := l.id
	for {
		id = id + 1
		if id < 10 {
			id = 10
		}
		_, bHave := l.sessionIDs[id]
		if bHave == false {
			l.id = id
			break
		}
	}
	return id
}

func (l *Listener) NewUDPMakeSession(remote *net.UDPAddr) *UDPMakeSession {

	addrStr := remote.String()

	session := &UDPMakeSession{UDPContext: UDPContext{remote: remote, sock: l.sock}, l: l, waitData: make(chan bool, 1), readCache: list.New(), RWMutex: sync.RWMutex{}, quitChan: make(chan bool, 1)}

	l.Lock()
	id := l.newID()
	l.sessions[addrStr] = session
	l.sessionIDs[id] = session
	l.Unlock()

	session.id = id

	kcp := ikcp.Ikcp_create(id, &session.UDPContext)
	kcp.Output = udp_output
	ikcp.Ikcp_wndsize(kcp, 128, 128)
	ikcp.Ikcp_nodelay(kcp, 1, 10, 2, 1)

	session.kcp = kcp

	session.lastRead = time.Now()
	session.lastWrite = time.Now()

	return session
}

func NewUDPMakeSession(remote *net.UDPAddr, local *net.UDPConn, id uint32) *UDPMakeSession {

	session := &UDPMakeSession{UDPContext: UDPContext{remote: remote, sock: local}, id: id, waitData: make(chan bool, 1), readCache: list.New(), RWMutex: sync.RWMutex{}, quitChan: make(chan bool, 1)}

	kcp := ikcp.Ikcp_create(id, &session.UDPContext)
	kcp.Output = udp_output
	ikcp.Ikcp_wndsize(kcp, 128, 128)
	ikcp.Ikcp_nodelay(kcp, 1, 10, 2, 1)

	session.kcp = kcp

	session.lastRead = time.Now()
	session.lastWrite = time.Now()

	return session
}

func Listen(addr string) (*Listener, error) {
	return newListener(addr)
}

func (l *Listener) recvLoop() {

	// defer SavePanic()

	buf := make([]byte, 5000)

	for {

		if l.close == true {
			break
		}

		n, from, err := l.sock.ReadFromUDP(buf)
		if n > 0 && err == nil {

			addr := from.String()
			conv := binary.LittleEndian.Uint32(buf)
			l.Lock()
			session, bHave := l.sessions[addr]
			l.Unlock()

			if conv == uint32(FirstSYN) {

				if bHave == false {
					session = l.NewUDPMakeSession(from)
					//log.Println("FirstSYN:", session.id)

					l.waitQueueLock.Lock()
					l.waitQueue.PushBack(session)
					l.waitQueueLock.Unlock()

					select {
					case l.newSession <- true:
					default:
					}

				}

				if session != nil {
					binary.LittleEndian.PutUint32(buf, uint32(SndSYN))
					binary.LittleEndian.PutUint32(buf[4:], session.id)
					l.sock.WriteToUDP(buf[:8], from)
				}

				continue
			}

			/* if conv == FirstACK {

				id := binary.LittleEndian.Uint32(buf[4:])
				log.Println("firstack:", id)
				l.Lock()
				session,bHave := l.sessionIDs[id]
				l.Unlock()
				if bHave {


					//这个有必要发 让对方dial完成 返回session
					binary.LittleEndian.PutUint32(buf, SndACK)
					binary.LittleEndian.PutUint32(buf[4:], session.id)
					l.sock.WriteToUDP(buf[:8], from)

				}

				continue
			} */

			if bHave {
				session.Lock()
				//if session.close == false {
				ikcp.Ikcp_input(session.kcp, buf, n)
				//}
				session.Unlock()
			}

		}

		if n < 0 {
			break
		}

		//log.Println("recvLoop")
	}

}

func (l *Listener) Accept() (*UDPMakeSession, error) {

	// defer SavePanic()

	var session *UDPMakeSession

	//log.Println("B Accept")
	for {

		session = nil

		l.waitQueueLock.Lock()
		lp := l.waitQueue.Front()
		if lp != nil {
			session = lp.Value.(*UDPMakeSession)
			l.waitQueue.Remove(lp)
		}
		l.waitQueueLock.Unlock()

		if session != nil {
			break
		}

		select {
		case <-l.newSession:
		case <-time.After(time.Millisecond * 300):
		case <-l.quitChan:
			return nil, errors.New("listener close")
		}

		//log.Println("Accept")

	}

	go session.updateLoop()
	//log.Println("e Accept, sid: ", session.id)

	return session, nil
}

func iclock() int32 {
	return int32((time.Now().UnixNano() / 1000000) & 0xffffffff)
}

func Dial(remote string) (*UDPMakeSession, error) {
	//log.Println("Dial ", remote)
	// defer SavePanic()
	udpAddr, err := net.ResolveUDPAddr("udp", remote)
	if err != nil {
		return nil, err
	}

	sock, _err := net.ListenUDP("udp", &net.UDPAddr{})
	if _err != nil {
		return nil, _err
	}

	var session *UDPMakeSession

	var id uint32
	//buf := make([]byte, 1500) //udp包大小不超过1400
	buf := make([]byte, 8)
	//var n int
	for j := 0; j < 20; j = j + 1 {
		binary.LittleEndian.PutUint32(buf, uint32(FirstSYN))
		sock.WriteTo(buf[:8], udpAddr)
		sock.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_n, from, err := sock.ReadFromUDP(buf)
		//log.Println(_n, from, err)
		//n = _n
		if _n >= 8 {
			conv := binary.LittleEndian.Uint32(buf)
			id = binary.LittleEndian.Uint32(buf[4:])
			if id >= 10 && conv == uint32(SndSYN) {
				session = NewUDPMakeSession(udpAddr, sock, id)
				//binary.LittleEndian.PutUint32(buf, FirstACK)
				//binary.LittleEndian.PutUint32(buf, id)
				//j = 0
				//continue
				break
			}
			//else if conv >= 10 { //可能主动发过来
			//	id = conv
			//	session = NewUDPMakeSession(udpAddr, sock, id)
			//	break
			//}

			//if id >= 10 && conv == SndACK {

			//	break
			//}
		}
		if _n < 0 {
			sock.Close()
			return nil, err
		}
		if j >= 19 {
			sock.Close()
			return nil, errors.New("dial: no response")
		}
		_ = from
	}

	go session.readLoop()
	go session.updateLoop()

	//log.Println("Dial end, id: ", id)
	return session, nil
}

func (session *UDPMakeSession) readLoop() {
	// defer SavePanic()
	buf := make([]byte, 8000)
	for {
		//if session.close == true {
		//log.Println(session.id, "readLoop should close")
		//	break
		//}
		session.sock.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, from, err := session.sock.ReadFromUDP(buf)
		if err == nil && n > 0 {
			//log.Println("readLoop:", n, binary.LittleEndian.Uint32(buf))
			session.Lock()
			if binary.LittleEndian.Uint32(buf) >= 10 {
				ikcp.Ikcp_input(session.kcp, buf, n)
			}
			session.Unlock()
		}
		_ = from
		if n < 0 {
			//log.Println("ReadFromUDP error:", err)
			break
		}

		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() == false {
				//log.Println("readLoop", err)
				break
			}
		}

		//log.Println("readLoop", err)
	}

}

func (session *UDPMakeSession) kcpRecv(buf []byte) {
	for {
		n := ikcp.Ikcp_recv(session.kcp, buf, 10000)
		if n <= 0 {
			break
		}
		//log.Println("Ikcp_recv:", n, buf[0])
		nbuf := make([]byte, n)
		copy(nbuf, buf[:n])
		session.readCache.PushBack(nbuf)

		//log.Println(session.id, "select wait")
		select {
		case <-session.quitChan:
		case session.waitData <- true:
		default:
		}
		//log.Println(session.id, "select wait e")
		//log.Println("kcpRecv")
		// if session.close == false {
		// select {
		// case session.waitData <- true :
		// default:
		// }
		// }

	}
}

//update、read整一个包和超时关闭
func (session *UDPMakeSession) updateLoop() {
	// defer SavePanic()
	buf := make([]byte, 10000)

	var i uint32

	for {

		if session.close == true {
			//log.Println(session.id, "close loop b")
			for j := 0; j < 20; j = j + 1 {
				session.Lock()
				ikcp.Ikcp_update(session.kcp, uint32(iclock()))
				session.kcpRecv(buf)
				session.Unlock()
				time.Sleep(20 * time.Millisecond)
			}
			if session.l != nil {
				session.l.Lock()
				delete(session.l.sessions, session.remote.String())
				delete(session.l.sessionIDs, session.id)
				session.l.Unlock()
			} else {
				session.sock.Close()
			}
			//log.Println(session.id, "close loop e")
			break
		}

		session.Lock()
		ikcp.Ikcp_update(session.kcp, uint32(iclock()))

		i = i + 1

		//if i % 2 == 0 {
		session.kcpRecv(buf)
		//}
		session.Unlock()

		if i%100 == 0 {

			if time.Since(session.lastRead).Seconds() > 6 && time.Since(session.lastWrite).Seconds() > 6 {
				//log.Println(session.id, "timeout close session")
				session.Close()
			}

		}

		time.Sleep(10 * time.Millisecond)

	}

}

func (session *UDPMakeSession) Read(p []byte) (n int, err error) {
	// defer SavePanic()
	//这个不好 要继续让程序读 直到读完缓冲
	//if session.close == true {
	//	return -1, errors.New("socket close")
	//}

	for {

		var cmd byte = 0

		session.Lock()

		lp := session.readCache.Front()

		if lp != nil {

			tmp := lp.Value.([]byte)

			if session.readCacheReaded < session.readCacheLen {
				n = len(p)
				if n > session.readCacheLen-session.readCacheReaded {
					n = session.readCacheLen - session.readCacheReaded
				}
				copy(p, tmp[session.readCacheReaded:session.readCacheReaded+n])
				session.readCacheReaded = session.readCacheReaded + n

			} else {
				hr := len(tmp)
				cmd = tmp[0]
				n = len(p)
				if n > hr-1 { //缓冲大于接收
					n = hr - 1
				} else if n < hr-1 { //接收大于缓冲
					session.readCacheReaded = n + 1
					session.readCacheLen = hr
				}
				if n > 0 {
					copy(p, tmp[1:n+1])
				}

			}
			//log.Println(session.id, n, session.readCacheReaded, session.readCacheLen)
			if session.readCacheReaded >= session.readCacheLen {

				session.readCache.Remove(lp)
			}

			session.lastRead = time.Now()

		}
		session.Unlock()

		if cmd == Close {
			session._Close()
			return 0, errors.New("cmd=close socket close")
		}

		if n == 0 {
			//log.Println(session.id, "wait")
			if session.close == true {
				//log.Println(session.id, "wait ret")
				return 0, errors.New("n=0 socket close")
			}
			//log.Println(session.id, "select wait")
			select {
			case <-session.quitChan:
			case <-session.waitData:
			}
			//<- session.waitData
			//log.Println(session.id, "select wait")
			//log.Println(session.id, "wait new loop")
		} else {
			break
		}

	}

	return n, nil

}

/*
func checkErr(err error) {
  if err == nil {
    println("Ok")
  } else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
    println("Timeout")
  } else if opError, ok := err.(*net.OpError); ok {
    if opError.Op == "dial" {
      println("Unknown host")
    } else if opError.Op == "read" {
      println("Connection refused")
    }
  }
}
*/

func (session *UDPMakeSession) Write(b []byte) (n int, err error) {
	// defer SavePanic()
	sendL := len(b)
	if sendL == 0 {
		return 0, nil
	}
	if session.close == true {
		return 0, errors.New("socket close")
	}

	for {
		if ikcp.Ikcp_waitsnd(session.kcp) > dataLimit {
			time.Sleep(40 * time.Millisecond)
		} else {
			break
		}
	}

	data := make([]byte, sendL+1)
	data[0] = Data
	copy(data[1:], b)
	//log.Println(session.id, "write:", sendL+1)
	session.Lock()
	ikcp.Ikcp_send(session.kcp, data, sendL+1)
	session.lastWrite = time.Now()
	session.Unlock()

	return sendL, err
}

func (session *UDPMakeSession) _Close() error {

	if session.close == true {
		return nil
	}

	session.close = true
	close(session.quitChan)

	return nil
}

//send close cmd  虽然有超时机制 还是有必要发送关闭命令关闭远端
func (session *UDPMakeSession) Close() error {

	if session.close == true {
		return nil
	}

	session.close = true
	close(session.quitChan)

	data := []byte{Close}
	session.Lock()
	ikcp.Ikcp_send(session.kcp, data, 1)
	session.Unlock()

	//不要在这里删除session
	//session.l.Lock()
	//delete(session.l.sessions, session.id)
	//session.l.Unlock()

	return nil
}

func (session *UDPMakeSession) LocalAddr() net.Addr {
	return session.sock.LocalAddr()
}

func (session *UDPMakeSession) RemoteAddr() net.Addr {
	return net.Addr(session.remote)
}

func (session *UDPMakeSession) SetDeadline(t time.Time) error {
	//return session.sock.SetDeadline(t)
	if session.l == nil {
		//return session.sock.SetDeadline(t)
	}
	return nil
}

func (session *UDPMakeSession) SetReadDeadline(t time.Time) error {
	//return session.sock.SetReadDeadline(t)
	if session.l == nil {
		//return session.sock.SetReadDeadline(t)
	}
	return nil
}

func (session *UDPMakeSession) SetWriteDeadline(t time.Time) error {
	//return session.sock.SetWriteDeadline(t)
	if session.l == nil {
		//return session.sock.SetWriteDeadline(t)
	}
	return nil
}

func (l *Listener) Close() error {
	l.close = true
	if l.sock != nil {
		l.sock.Close()
		//l.sock = nil
	}
	close(l.quitChan)
	//log.Println("listener close")
	return nil
}

func (l *Listener) Addr() net.Addr {
	return nil
}
