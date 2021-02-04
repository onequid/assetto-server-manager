package acserver

import (
	"net"
)

type SessionInfoHandler struct {
	state          *ServerState
	sessionManager *SessionManager
}

func NewSessionInfoHandler(state *ServerState, sessionManager *SessionManager) *SessionInfoHandler {
	return &SessionInfoHandler{
		state:          state,
		sessionManager: sessionManager,
	}
}

func (s SessionInfoHandler) OnMessage(_ net.PacketConn, addr net.Addr, p *Packet) error {
	car := s.state.GetCarByUDPAddress(addr)

	if car == nil {
		return nil
	}

	gameThinksWeAreInSessionType := SessionType(p.ReadUint8())
	currentSession := s.sessionManager.GetCurrentSession()

	if gameThinksWeAreInSessionType == currentSession.SessionType {
		return nil
	}

	s.sessionManager.SendSessionInfo(car, nil)

	return nil
}

func (s SessionInfoHandler) MessageType() MessageType {
	return UDPMessageSessionInfo
}