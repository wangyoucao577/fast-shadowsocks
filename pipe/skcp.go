package pipe

import (
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/dearplain/fast-shadowsocks/ikcp"
)

const dataLimit = 60

const (
	CmdClose byte = 2 + iota
	CmdCreateSession
	CmdCreateSessionRsp
	CmdData byte = 0xee
)

var xorConst = []byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}

func udp_output(buf []byte, _len int32, kcp *ikcp.Ikcpcb, user interface{}) int32 {
	c := (user).(*UDPContext)
	buf = buf[:_len]
	//newBuf := make([]byte, len(buf))
	//xorWithConst(newBuf, buf)

	_, err := c.localSock.WriteToUDP(buf, c.remote)
	if err != nil {
		//log.Println("udp_output", err)
	}
	return 0
}

type UDPContext struct {
	localSock *net.UDPConn
	remote    *net.UDPAddr
}

type UDPSession struct {
	UDPContext
	sync.RWMutex
	readLock        sync.RWMutex
	writeLock       sync.RWMutex
	listener        *Listener
	localAddr       *net.UDPAddr
	kcp             *ikcp.Ikcpcb
	close           bool
	waitData        chan bool
	quitChan        chan bool
	readTempBuf     []byte
	readCacheLen    int
	readCacheReaded int
	readRemain      int
	readChan        chan []byte
	lastRead        time.Time
	lastWrite       time.Time
	//waitingData     bool
	//readCache       *list.List
}

type Listener struct {
	sync.RWMutex
	sock        *net.UDPConn
	remoteAddrs map[string]*UDPSession
	close       bool
	newSession  chan *UDPSession
	quitChan    chan bool
}

type Action struct {
	c   chan bool
	arg interface{}
}

/*
func SavePanic() {
	r := recover()
	if r == nil {
		return
	}

	buf := make([]byte, 10000)
	buf = buf[:runtime.Stack(buf, false)]

	ioutil.WriteFile("err.txt", buf, 0644)
	os.Exit(1)
}
*/

func newListener(local string) (*Listener, error) {

	//var bb = make([]byte, 10)
	//var re = make([]byte, 10)
	//xorBytes(bb, []byte{3, 1, 2, 2, 5, 5}, []byte{1, 2, 3, 4})
	//xorBytes(re, []byte{1, 2, 3, 4}, bb[:6])
	//xorWithConst(re, []byte{3, 1, 2, 2, 5, 5})
	//xorWithConst(re, re[:6])

	listener := &Listener{
		remoteAddrs: make(map[string]*UDPSession),
		RWMutex:     sync.RWMutex{},
		newSession:  make(chan *UDPSession, 20),
		quitChan:    make(chan bool),
	}

	if local != "" {
		udpAddr, err := net.ResolveUDPAddr("udp", local)
		if err != nil {
			return nil, err
		}
		sock, _err := net.ListenUDP("udp", udpAddr)
		if _err != nil {
			return nil, _err
		}
		listener.sock = sock
	} else {
		sock, _err := net.ListenUDP("udp", &net.UDPAddr{})
		if _err != nil {
			return nil, _err
		}
		listener.sock = sock
	}

	go listener.recvLoop()

	return listener, nil
}

func (listener *Listener) newUDPSession(remote *net.UDPAddr) (*UDPSession, bool) {

	remoteAddrStr := remote.String()
	listener.Lock()
	if sess, ok := listener.remoteAddrs[remoteAddrStr]; ok == true {
		listener.Unlock()
		return sess, true
	}
	listener.Unlock()

	var err error
	var local *net.UDPConn
	if local, err = net.ListenUDP("udp", &net.UDPAddr{}); err != nil {
		//log.Println(err)
		return nil, false
	}
	localAddr := local.LocalAddr().(*net.UDPAddr)

	sess := newUDPSession(listener, remote, local, localAddr)

	listener.Lock()
	listener.remoteAddrs[remoteAddrStr] = sess
	listener.Unlock()

	return sess, false
}

func newUDPSession(listener *Listener, remote *net.UDPAddr, local *net.UDPConn, localAddr *net.UDPAddr) *UDPSession {

	//log.Println("newUDPSession remote", remote, "local", localAddr)

	session := &UDPSession{
		UDPContext: UDPContext{
			remote:    remote,
			localSock: local,
		},
		listener:  listener,
		localAddr: localAddr,
		waitData:  make(chan bool, 1),
		readChan:  make(chan []byte, 50),
		RWMutex:   sync.RWMutex{},
		readLock:  sync.RWMutex{},
		writeLock: sync.RWMutex{},
		quitChan:  make(chan bool),
		lastRead:  time.Now(),
		lastWrite: time.Now(),
		//readCache: list.New(),
	}

	kcp := ikcp.Ikcp_create(0xeeeeeeee, &session.UDPContext)
	kcp.Output = udp_output
	ikcp.Ikcp_wndsize(kcp, 128, 128)
	ikcp.Ikcp_nodelay(kcp, 1, 10, 2, 1)
	session.kcp = kcp

	return session
}

func Listen(addr string) (*Listener, error) {
	return newListener(addr)
}

func (listener *Listener) recvLoop() {

	// defer SavePanic()
	//log.Println("recvLoop")
	rbuf := make([]byte, 2000)
	wbuf := make([]byte, 5)

	for {
		if listener.close == true {
			break
		}
		if n, from, err := listener.sock.ReadFromUDP(rbuf); n > 0 && err == nil {
			cmd := rbuf[0]
			if cmd == CmdCreateSession {
				newFrom := &net.UDPAddr{
					IP:   from.IP,
					Port: int(binary.LittleEndian.Uint32(rbuf[1:])),
				}
				session, isOld := listener.newUDPSession(newFrom)
				if session != nil {
					if isOld == false {
						select {
						case listener.newSession <- session:
							//("listener.newSession <- session")
							//log.Println("session.localAddr", session.localAddr)
						case <-listener.quitChan:
							return
						}
					}
					wbuf[0] = CmdCreateSessionRsp
					binary.LittleEndian.PutUint32(wbuf[1:], uint32(session.localAddr.Port))
					listener.sock.WriteToUDP(wbuf, from)
				}
			}
			rbuf[0] = 0
			binary.LittleEndian.PutUint32(rbuf[1:], 0)
		} else if n < 0 {
			break
		}
	}

}

func (listener *Listener) Accept() (*UDPSession, error) {
	//log.Println("Accept b")
	select {
	case session := <-listener.newSession:
		go session.readLoop()
		go session.updateLoop()
		//log.Println("Accept e")
		return session, nil
	case <-listener.quitChan:
		return nil, errors.New("listener close")
	}
}

func (listener *Listener) Close() error {
	if listener.close {
		return nil
	}
	listener.close = true
	//log.Println("Close")
	if listener.sock != nil {
		listener.sock.Close()
	}
	close(listener.quitChan)
	return nil
}

func (l *Listener) Addr() net.Addr {
	return l.sock.LocalAddr()
}

func iclock() int32 {
	return int32((time.Now().UnixNano() / 1000000) & 0xffffffff)
}

func Dial(remote string) (*UDPSession, error) {
	// defer SavePanic()
	//log.Println("Dial")
	remoteAddr, err := net.ResolveUDPAddr("udp", remote)
	if err != nil {
		return nil, err
	}

	tmpSock, _err := net.ListenUDP("udp", nil)
	if _err != nil {
		return nil, _err
	}

	// 必须新建一个 否则服务端新socket发送过来的数据 会直接被丢弃 接收不到
	localSock, _err := net.ListenUDP("udp", nil)
	if _err != nil {
		return nil, _err
	}
	localAddr := localSock.LocalAddr().(*net.UDPAddr)

	//buf := make([]byte, 1500) //udp包大小不超过1400
	wbuf := make([]byte, 5)
	buf := make([]byte, 5)
	wbuf[0] = CmdCreateSession
	binary.LittleEndian.PutUint32(wbuf[1:], uint32(localAddr.Port))
	for j := 0; j < 20; j = j + 1 {
		tmpSock.WriteTo(wbuf, remoteAddr)
		tmpSock.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, from, err := tmpSock.ReadFromUDP(buf)
		if n >= len(buf) && from.String() == remoteAddr.String() {
			if buf[0] == CmdCreateSessionRsp {
				//log.Println("CmdCreateSessionRsp")
				tmpSock.Close()
				newAddr := net.UDPAddr{
					IP:   from.IP,
					Port: int(binary.LittleEndian.Uint32(buf[1:])),
				}
				session := newUDPSession(nil, &newAddr, localSock, localAddr)
				go session.readLoop()
				go session.updateLoop()
				return session, nil
			}
		}
		if n < 0 {
			//log.Println("dial: n < 0 ")
			tmpSock.Close()
			localSock.Close()
			return nil, err
		}
	}

	tmpSock.Close()
	localSock.Close()
	return nil, errors.New("dial: no response")
}

func xorWithConst(dst, src []byte) {
	n := len(src)
	dst = dst[:n]
	xlen := len(xorConst)
	count := n/xlen + 1
	for i := 0; i < count; i++ {
		xorBytes(dst[i*xlen:], xorConst, src[i*xlen:])
	}
}

func (session *UDPSession) readLoop() {
	// defer SavePanic()

	buf := make([]byte, 20*1024)
	//ebuf := make([]byte, 20*1024)
	for {
		//log.Println("readLoop")
		if session.close == true {
			break
		}
		session.localSock.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, from, err := session.localSock.ReadFromUDP(buf)
		if err == nil && n > 0 {

			//xorWithConst(buf, ebuf)

			cmd := buf[0]
			//log.Println("cmd", cmd)
			switch cmd {
			case CmdData:
				//log.Println("CmdData", cmd)
				session.Lock()
				ikcp.Ikcp_input(session.kcp, buf, n)
				//ikcp.Ikcp_update(session.kcp, uint32(iclock()))
				//rn := session.kcpRecv(buf)
				//session.kcpRecv(buf)
				session.Unlock()
				session.kcpRecv(buf)
				//if rn > 0 {
				//	log.Println("kcpRecv", n)
				//}
			case CmdClose:
				session.Close()
				return
			}

			//for {
			//	if session.readRemain > 50*1024 {
			//log.Println("readLoop wait")
			//		time.Sleep(10 * time.Millisecond)
			//	} else {
			//		break
			//	}
			//}

		}

		_ = from
		if n < 0 {
			break
		}
		if err != nil {
			//log.Println("readLoop err", err)
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() == false {
				// 不是timeout
				break
			}
		}
	}

}

func (session *UDPSession) kcpRecv(buf []byte) {
	for {
		session.Lock()
		n := ikcp.Ikcp_recv(session.kcp, buf, 10000)
		session.Unlock()
		if n <= 0 {
			break
		}
		nbuf := make([]byte, n)
		copy(nbuf, buf[:n])
		//log.Println("kcpRecv", n)

		session.readChan <- nbuf
	}
}

/*
func (session *UDPSession) kcpRecv(buf []byte) {
	for {
		n := ikcp.Ikcp_recv(session.kcp, buf, 10000)
		if n <= 0 {
			break
		}
		nbuf := make([]byte, n)
		copy(nbuf, buf[:n])
		session.readCache.PushBack(nbuf)
		session.readRemain = session.readRemain + int(n)
		//log.Println("kcpRecv", n)
		if session.waitingData == true {
			select {
			case session.waitData <- true:
			default:
			}
		}
		//select {
		//case session.waitData <- true:
		//default:
		//}
	}
}
*/

func (session *UDPSession) updateLoop() {
	// defer SavePanic()
	//buf := make([]byte, 20*1024)
	var i uint32
	for {
		//log.Println("updateLoop")
		if session.close == true {
			break
		}
		session.Lock()
		ikcp.Ikcp_update(session.kcp, uint32(iclock()))
		//session.kcpRecv(buf)
		session.Unlock()
		//session.kcpRecv(buf)  //不要卡住 卡住就无法update了
		i = i + 1
		if i%100 == 0 {
			if time.Since(session.lastRead).Seconds() > 30 && time.Since(session.lastWrite).Seconds() > 30 {
				session.Close()
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

/*
func (session *UDPSession) Read(p []byte) (n int, err error) {
	// defer SavePanic()

	if len(p) <= 0 {
		return 0, nil
	}

	for {
		//log.Println("Read")
		n = 0
		var tmp []byte

		session.Lock()

		lp := session.readCache.Front()

		if lp != nil {

			tmp = lp.Value.([]byte)
			if session.readCacheReaded < session.readCacheLen {
				// 旧缓冲还没读完
				n = len(p)
				if n > session.readCacheLen-session.readCacheReaded {
					// 缓冲大于将要读取的数据
					n = session.readCacheLen - session.readCacheReaded
				}
				copy(p, tmp[session.readCacheReaded:session.readCacheReaded+n])
				session.readCacheReaded = session.readCacheReaded + n
			} else {
				// 这是一个新缓冲
				hr := len(tmp)
				n = len(p)
				if n > hr { //能装下 n太大
					n = hr
				} else if n <= hr { //不能装下 n够小 n值不需要改变
					session.readCacheReaded = n
					session.readCacheLen = hr
				}

				if n > 0 {
					copy(p, tmp[:n])
				}

			}

			if session.readCacheReaded >= session.readCacheLen {
				session.readCache.Remove(lp)
			}
			session.lastRead = time.Now()

		}
		if n > 0 {
			session.readRemain = session.readRemain - n
		}
		session.Unlock()

		if n == 0 {
			if session.close == true {
				return 0, errors.New("n=0 socket close")
			}
			//log.Println("read wait")
			session.waitingData = true
			select {
			case <-session.waitData:
				session.waitingData = false
			case <-session.quitChan:
				return 0, errors.New("n=0 socket close")
			}
			//time.Sleep(10 * time.Millisecond)
		} else {
			break
		}

	}

	return n, nil

}
*/

func (session *UDPSession) Read(p []byte) (n int, err error) {
	// defer SavePanic()

	if len(p) <= 0 {
		return 0, nil
	}

	session.readLock.Lock()
	if session.readCacheReaded < session.readCacheLen {
		// 旧缓冲还没读完
		var tmp = session.readTempBuf
		n = len(p)
		if n > session.readCacheLen-session.readCacheReaded {
			// 缓冲大于将要读取的数据
			n = session.readCacheLen - session.readCacheReaded
		}
		copy(p, tmp[session.readCacheReaded:session.readCacheReaded+n])
		session.readCacheReaded = session.readCacheReaded + n
	} else {

		var tmp []byte
		select {
		case tmp = <-session.readChan: // 用session.readLock 而不是session.Lock 保证读的顺序
			session.readTempBuf = tmp
		case <-session.quitChan:
			session.readLock.Unlock()
			return 0, errors.New("n=0 socket close")
		}

		// 这是一个新缓冲
		hr := len(tmp)
		n = len(p)
		if n > hr { //能装下 n太大
			n = hr
		} else if n <= hr { //不能装下 n够小 n值不需要改变
			session.readCacheReaded = n
			session.readCacheLen = hr
		}

		if n > 0 {
			copy(p, tmp[:n])
		}

	}
	session.lastRead = time.Now()
	if n > 0 {
		session.readRemain = session.readRemain - n
	}

	session.readLock.Unlock()

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

func (session *UDPSession) Write(data []byte) (n int, err error) {
	// defer SavePanic()
	sendL := len(data)
	if sendL == 0 {
		return 0, nil
	}
	if session.close == true {
		return 0, errors.New("socket close")
	}

	session.writeLock.Lock()
	for {
		if ikcp.Ikcp_waitsnd(session.kcp) > dataLimit {
			//log.Println("Write wait")
			time.Sleep(20 * time.Millisecond)
		} else {
			break
		}
	}

	//log.Println("Write", sendL)
	session.Lock()
	ikcp.Ikcp_send(session.kcp, data, sendL)
	session.lastWrite = time.Now()
	session.Unlock()
	session.writeLock.Unlock()

	return sendL, err
}

func (session *UDPSession) Close() error {
	//log.Println("Close", session.localAddr)

	session.localSock.WriteTo([]byte{CmdClose}, session.remote)

	session.Lock()
	if session.close == true {
		session.Unlock()
		return nil
	}
	session.close = true
	close(session.quitChan)
	session.Unlock()

	session.localSock.Close()

	if session.listener != nil {
		lsn := session.listener
		lsn.Lock()
		delete(session.listener.remoteAddrs, session.remote.String())
		session.listener = nil
		lsn.Unlock()
	}

	return nil
}

func (session *UDPSession) LocalAddr() net.Addr {
	return session.localSock.LocalAddr()
}

func (session *UDPSession) RemoteAddr() net.Addr {
	return net.Addr(session.remote)
}

func (session *UDPSession) SetDeadline(t time.Time) error {
	//return session.sock.SetDeadline(t)
	if session.listener == nil {
		//return session.sock.SetDeadline(t)
	}
	return nil
}

func (session *UDPSession) SetReadDeadline(t time.Time) error {
	//return session.sock.SetReadDeadline(t)
	return nil
}

func (session *UDPSession) SetWriteDeadline(t time.Time) error {
	//return session.sock.SetWriteDeadline(t)
	return nil
}

const wordSize = int(unsafe.Sizeof(uintptr(0)))
const supportsUnaligned = runtime.GOARCH == "386" || runtime.GOARCH == "amd64"

// fastXORBytes xors in bulk. It only works on architectures that
// support unaligned read/writes.
func fastXORBytes(dst, a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}

	w := n / wordSize
	if w > 0 {
		dw := *(*[]uintptr)(unsafe.Pointer(&dst))
		aw := *(*[]uintptr)(unsafe.Pointer(&a))
		bw := *(*[]uintptr)(unsafe.Pointer(&b))
		for i := 0; i < w; i++ {
			dw[i] = aw[i] ^ bw[i]
		}
	}

	for i := (n - n%wordSize); i < n; i++ {
		dst[i] = a[i] ^ b[i]
	}

	return n
}

func safeXORBytes(dst, a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dst[i] = a[i] ^ b[i]
	}
	return n
}

// xorBytes xors the bytes in a and b. The destination is assumed to have enough
// space. Returns the number of bytes xor'd.
func xorBytes(dst, a, b []byte) int {
	if supportsUnaligned {
		return fastXORBytes(dst, a, b)
	} else {
		// TODO(hanwen): if (dst, a, b) have common alignment
		// we could still try fastXORBytes. It is not clear
		// how often this happens, and it's only worth it if
		// the block encryption itself is hardware
		// accelerated.
		return safeXORBytes(dst, a, b)
	}
}

// fastXORWords XORs multiples of 4 or 8 bytes (depending on architecture.)
// The arguments are assumed to be of equal length.
func fastXORWords(dst, a, b []byte) {
	dw := *(*[]uintptr)(unsafe.Pointer(&dst))
	aw := *(*[]uintptr)(unsafe.Pointer(&a))
	bw := *(*[]uintptr)(unsafe.Pointer(&b))
	n := len(b) / wordSize
	for i := 0; i < n; i++ {
		dw[i] = aw[i] ^ bw[i]
	}
}

func xorWords(dst, a, b []byte) {
	if supportsUnaligned {
		fastXORWords(dst, a, b)
	} else {
		safeXORBytes(dst, a, b)
	}
}
