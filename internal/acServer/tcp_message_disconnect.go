package acServer

import (
	"net"
)

type DisconnectMessageHandler struct {
	state *ServerState
}

func NewDisconnectMessageHandler(state *ServerState) *DisconnectMessageHandler {
	return &DisconnectMessageHandler{state: state}
}

func (d *DisconnectMessageHandler) OnMessage(conn net.Conn, p *Packet) error {
	car, err := d.state.GetCarByTCPConn(conn)

	if err == ErrCarNotFound {
		closeTCPConnection(conn)
		return nil
	} else if err != nil {
		return err
	}

	return d.state.DisconnectCar(car)
}

func (d *DisconnectMessageHandler) MessageType() MessageType {
	return TCPMessageDisconnect
}
