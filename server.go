package pingtunnel

import (
	"github.com/esrrhs/go-engine/src/loggo"
	"github.com/golang/protobuf/proto"
	"golang.org/x/net/icmp"
	"net"
	"time"
)

func NewServer(timeout int, key int) (*Server, error) {
	return &Server{
		timeout: timeout,
		key:     key,
	}, nil
}

type Server struct {
	timeout int
	key     int

	conn *icmp.PacketConn

	localConnMap map[string]*ServerConn

	sendPacket     uint64
	recvPacket     uint64
	sendPacketSize uint64
	recvPacketSize uint64

	echoId  int
	echoSeq int
}

type ServerConn struct {
	ipaddrTarget  *net.UDPAddr
	conn          *net.UDPConn
	tcpaddrTarget *net.TCPAddr
	tcpconn       *net.TCPConn
	id            string
	activeTime    time.Time
	close         bool
	rproto        int
	fm            *FrameMgr
	tcpmode       int
}

func (p *Server) Run() {

	conn, err := icmp.ListenPacket("ip4:icmp", "")
	if err != nil {
		loggo.Error("Error listening for ICMP packets: %s", err.Error())
		return
	}
	p.conn = conn

	p.localConnMap = make(map[string]*ServerConn)

	recv := make(chan *Packet, 10000)
	go recvICMP(*p.conn, recv)

	interval := time.NewTicker(time.Second)
	defer interval.Stop()

	for {
		select {
		case <-interval.C:
			p.checkTimeoutConn()
			p.showNet()
		case r := <-recv:
			p.processPacket(r)
		}
	}
}

func (p *Server) processPacket(packet *Packet) {

	if packet.my.Key != (int32)(p.key) {
		return
	}

	p.echoId = packet.echoId
	p.echoSeq = packet.echoSeq

	if packet.my.Type == (int32)(MyMsg_PING) {
		t := time.Time{}
		t.UnmarshalBinary(packet.my.Data)
		loggo.Info("ping from %s %s %d %d %d", packet.src.String(), t.String(), packet.my.Rproto, packet.echoId, packet.echoSeq)
		sendICMP(packet.echoId, packet.echoSeq, *p.conn, packet.src, "", "", (uint32)(MyMsg_PING), packet.my.Data,
			(int)(packet.my.Rproto), -1, p.key,
			0, 0, 0, 0)
		return
	}

	loggo.Debug("processPacket %s %s %d", packet.my.Id, packet.src.String(), len(packet.my.Data))

	now := time.Now()

	id := packet.my.Id
	localConn := p.localConnMap[id]
	if localConn == nil {

		if packet.my.Tcpmode > 0 {

			addr := packet.my.Target
			ipaddrTarget, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				loggo.Error("Error ResolveUDPAddr for tcp addr: %s %s", addr, err.Error())
				return
			}

			targetConn, err := net.DialTCP("tcp", nil, ipaddrTarget)
			if err != nil {
				loggo.Error("Error listening for tcp packets: %s", err.Error())
				return
			}

			fm := NewFrameMgr((int)(packet.my.TcpmodeBuffersize), (int)(packet.my.TcpmodeMaxwin), (int)(packet.my.TcpmodeResendTimems))

			localConn = &ServerConn{tcpconn: targetConn, tcpaddrTarget: ipaddrTarget, id: id, activeTime: now, close: false,
				rproto: (int)(packet.my.Rproto), fm: fm, tcpmode: (int)(packet.my.Tcpmode)}

			p.localConnMap[id] = localConn

			go p.RecvTCP(localConn, id, packet.src)

		} else {

			addr := packet.my.Target
			ipaddrTarget, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				loggo.Error("Error ResolveUDPAddr for udp addr: %s %s", addr, err.Error())
				return
			}

			targetConn, err := net.DialUDP("udp", nil, ipaddrTarget)
			if err != nil {
				loggo.Error("Error listening for udp packets: %s", err.Error())
				return
			}

			localConn = &ServerConn{conn: targetConn, ipaddrTarget: ipaddrTarget, id: id, activeTime: now, close: false,
				rproto: (int)(packet.my.Rproto), tcpmode: (int)(packet.my.Tcpmode)}

			p.localConnMap[id] = localConn

			go p.Recv(localConn, id, packet.src)
		}
	}

	localConn.activeTime = now

	if packet.my.Type == (int32)(MyMsg_DATA) {

		if packet.my.Tcpmode > 0 {
			f := &Frame{}
			err := proto.Unmarshal(packet.my.Data, f)
			if err != nil {
				loggo.Error("Unmarshal tcp Error %s", err)
				return
			}

			localConn.fm.OnRecvFrame(f)

		} else {
			_, err := localConn.conn.Write(packet.my.Data)
			if err != nil {
				loggo.Error("WriteToUDP Error %s", err)
				localConn.close = true
				return
			}
		}

		p.recvPacket++
		p.recvPacketSize += (uint64)(len(packet.my.Data))
	}
}

func (p *Server) RecvTCP(conn *ServerConn, id string, src *net.IPAddr) {

	loggo.Info("server waiting target response %s -> %s %s", conn.tcpaddrTarget.String(), conn.id, conn.tcpconn.LocalAddr().String())

	bytes := make([]byte, 10240)

	for {
		left := conn.fm.GetSendBufferLeft()
		if left >= len(bytes) {
			conn.tcpconn.SetReadDeadline(time.Now().Add(time.Millisecond * 100))
			n, err := conn.tcpconn.Read(bytes)
			if err != nil {
				if neterr, ok := err.(*net.OpError); ok {
					if neterr.Timeout() {
						// Read timeout
						n = 0
					} else {
						loggo.Error("Error read tcp %s %s %s", conn.id, conn.tcpaddrTarget.String(), err)
						break
					}
				}
			}
			if n > 0 {
				conn.fm.WriteSendBuffer(bytes[:n])
			}
		}

		conn.fm.Update()

		sendlist := conn.fm.getSendList()

		now := time.Now()
		conn.activeTime = now

		for e := sendlist.Front(); e != nil; e = e.Next() {

			f := e.Value.(Frame)
			mb, err := proto.Marshal(&f)
			if err != nil {
				loggo.Error("Error tcp Marshal %s %s %s", conn.id, conn.tcpaddrTarget.String(), err)
				continue
			}

			sendICMP(p.echoId, p.echoSeq, *p.conn, src, "", id, (uint32)(MyMsg_DATA), mb,
				conn.rproto, -1, p.key,
				0, 0, 0, 0)

			p.sendPacket++
			p.sendPacketSize += (uint64)(len(mb))
		}
	}
}

func (p *Server) Recv(conn *ServerConn, id string, src *net.IPAddr) {

	loggo.Info("server waiting target response %s -> %s %s", conn.ipaddrTarget.String(), conn.id, conn.conn.LocalAddr().String())

	for {
		bytes := make([]byte, 2000)

		conn.conn.SetReadDeadline(time.Now().Add(time.Millisecond * 100))
		n, _, err := conn.conn.ReadFromUDP(bytes)
		if err != nil {
			if neterr, ok := err.(*net.OpError); ok {
				if neterr.Timeout() {
					// Read timeout
					continue
				} else {
					loggo.Error("ReadFromUDP Error read udp %s", err)
					conn.close = true
					return
				}
			}
		}

		now := time.Now()
		conn.activeTime = now

		sendICMP(p.echoId, p.echoSeq, *p.conn, src, "", id, (uint32)(MyMsg_DATA), bytes[:n],
			conn.rproto, -1, p.key,
			0, 0, 0, 0)

		p.sendPacket++
		p.sendPacketSize += (uint64)(n)
	}
}

func (p *Server) Close(conn *ServerConn) {
	if p.localConnMap[conn.id] != nil {
		conn.conn.Close()
		delete(p.localConnMap, conn.id)
	}
}

func (p *Server) checkTimeoutConn() {

	now := time.Now()
	for _, conn := range p.localConnMap {
		if conn.tcpmode > 0 {
			continue
		}
		diff := now.Sub(conn.activeTime)
		if diff > time.Second*(time.Duration(p.timeout)) {
			conn.close = true
		}
	}

	for id, conn := range p.localConnMap {
		if conn.tcpmode > 0 {
			continue
		}
		if conn.close {
			loggo.Info("close inactive conn %s %s", id, conn.ipaddrTarget.String())
			p.Close(conn)
		}
	}
}

func (p *Server) showNet() {
	loggo.Info("send %dPacket/s %dKB/s recv %dPacket/s %dKB/s",
		p.sendPacket, p.sendPacketSize/1024, p.recvPacket, p.recvPacketSize/1024)
	p.sendPacket = 0
	p.recvPacket = 0
	p.sendPacketSize = 0
	p.recvPacketSize = 0
}
