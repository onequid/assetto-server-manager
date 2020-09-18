package acserver

import (
	"net"
	"strings"
)

const (
	CurrentProtocolVersion uint16 = 202
)

type HandshakeMessageHandler struct {
	state            *ServerState
	sessionManager   *SessionManager
	entryListManager *EntryListManager
	weatherManager   *WeatherManager

	plugin Plugin
	logger Logger
}

func NewHandshakeMessageHandler(state *ServerState, sessionManager *SessionManager, entryListManager *EntryListManager, weatherManager *WeatherManager, plugin Plugin, logger Logger) *HandshakeMessageHandler {
	return &HandshakeMessageHandler{
		state:            state,
		sessionManager:   sessionManager,
		entryListManager: entryListManager,
		weatherManager:   weatherManager,
		plugin:           plugin,
		logger:           logger,
	}
}

func (m HandshakeMessageHandler) OnMessage(conn net.Conn, p *Packet) error {
	protocolVersion := p.ReadUint16()

	if protocolVersion != CurrentProtocolVersion {
		m.logger.Infof("Client attempted to connect with invalid protocol version: %d (wanted: %d)", protocolVersion, CurrentProtocolVersion)

		p := NewPacket(nil)
		p.Write(TCPHandshakeUnsupportedProtocol)
		p.Write(CurrentProtocolVersion)

		if err := p.WriteTCP(conn); err != nil {
			return err
		}

		closeTCPConnection(conn)
		return nil
	}

	guid := p.ReadString()
	driverName := p.ReadUTF32String()
	driverTeam := p.ReadUTF32String()
	nation := p.ReadString()
	carModel := p.ReadString()
	password := p.ReadString()

	if m.state.currentSession.SessionType == SessionTypeBooking {
		p := NewPacket(nil)
		p.Write(TCPHandshakeStillBooking)
		p.Write(uint32(m.sessionManager.RemainingSessionTime().Seconds()))

		return p.WriteTCP(conn)
	}

	// check blocklist
	for _, blockedGUID := range m.state.blockList {
		if blockedGUID == guid {
			m.logger.Infof("Driver: %s (%s) was rejected as their guid is in the block list", driverName, guid)
			return closeTCPConnectionWithError(conn, TCPHandshakeBlockListed)
		}
	}

	// check no join list
	if _, ok := m.state.noJoinList[guid]; ok {
		m.logger.Infof("Driver: %s (%s) was rejected as their guid is in the no join list (was previously kicked during this session)", driverName, guid)
		return closeTCPConnectionWithError(conn, TCPHandshakeBlockListed)
	}

	if m.state.serverConfig.Password != "" {
		if password != m.state.serverConfig.Password && password != m.state.serverConfig.AdminPassword {
			m.logger.Infof("Driver: %s (%s) got the server password wrong", driverName, guid)
			return closeTCPConnectionWithError(conn, TCPHandshakeWrongPassword)
		}
	}

	if !m.sessionManager.JoinIsAllowed(guid) {
		m.logger.Infof("Driver: %s (%s) tried to join but was rejected as current session is closed", driverName, guid)
		return closeTCPConnectionWithError(conn, TCPHandshakeSessionClosed)
	}

	driver := Driver{
		Name:   driverName,
		Team:   driverTeam,
		GUID:   guid,
		Nation: nation,
	}

	driverIsAdmin := m.state.serverConfig.AdminPassword != "" && password == m.state.serverConfig.AdminPassword

	entrant, err := m.entryListManager.ConnectCar(conn, driver, carModel, driverIsAdmin)

	if err == ErrNoAvailableSlots {
		m.logger.WithError(err).Errorf("Could not connect driver (%s/%s) to car.", driver.Name, driver.GUID)
		return closeTCPConnectionWithError(conn, TCPHandshakeNoSlotsAvailable)
	} else if err != nil {
		m.logger.WithError(err).Errorf("Could not connect driver (%s/%s) to car.", driver.Name, driver.GUID)
		return closeTCPConnectionWithError(conn, TCPHandshakeAuthFailed)
	}

	m.logger.Infof("Received handshake request from: %s", entrant.String())

	w := NewPacket(nil)

	w.Write(TCPHandshakeSuccess)
	w.WriteUTF32String(m.state.serverConfig.Name)
	w.Write(m.state.serverConfig.UDPPort)
	w.Write(m.state.serverConfig.ClientSendIntervalInHertz)
	w.WriteString(m.state.raceConfig.Track)
	w.WriteString(m.state.raceConfig.TrackLayout)
	w.WriteString(entrant.Model)
	w.WriteString(entrant.Skin)
	w.Write(m.weatherManager.sunAngle)
	w.Write(m.state.raceConfig.AllowedTyresOut)
	w.Write(m.state.raceConfig.TyreBlanketsAllowed)
	w.Write(m.state.raceConfig.TractionControlAllowed)
	w.Write(m.state.raceConfig.ABSAllowed)
	w.Write(m.state.raceConfig.StabilityControlAllowed)
	w.Write(m.state.raceConfig.AutoClutchAllowed)
	w.Write(m.state.raceConfig.StartRule)
	w.Write(m.state.raceConfig.DamageMultiplier / 100)
	w.Write(m.state.raceConfig.FuelRate / 100)
	w.Write(m.state.raceConfig.TyreWearRate / 100)
	w.Write(m.state.raceConfig.ForceVirtualMirror)
	w.Write(m.state.raceConfig.MaxContactsPerKilometer)
	w.Write(m.state.raceConfig.RaceOverTime)
	w.Write(m.state.raceConfig.ResultScreenTime * 1000)
	w.Write(m.state.raceConfig.RaceExtraLap)
	w.Write(m.state.raceConfig.RaceGasPenaltyDisabled)
	w.Write(m.state.raceConfig.RacePitWindowStart)
	w.Write(m.state.raceConfig.RacePitWindowEnd)
	w.Write(m.state.raceConfig.ReversedGridRacePositions)
	w.Write(entrant.CarID)

	sessions := m.state.raceConfig.InGameSessions()

	w.Write(uint8(len(sessions)))

	for _, session := range sessions {
		w.Write(session.SessionType)
		w.Write(session.Laps)
		w.Write(session.Time)
	}

	w.WriteString(m.state.currentSession.Name)
	w.Write(m.state.currentSessionIndex)
	w.Write(m.state.currentSession.SessionType)
	w.Write(m.state.currentSession.Time)
	w.Write(m.state.currentSession.Laps)
	w.Write(m.state.raceConfig.DynamicTrack.CurrentGrip)

	carPos := uint8(entrant.CarID)

	for pos, leaderboardLine := range m.state.Leaderboard() {
		if leaderboardLine.Car.CarID == entrant.CarID {
			carPos = uint8(pos)
			break
		}
	}

	w.Write(carPos)

	entrant.Driver.JoinTime = currentTimeMillisecond()
	w.Write(m.sessionManager.ElapsedSessionTime().Milliseconds())
	w.Write(uint8(len(m.state.checkSummableFiles)))

	m.logger.Infof("Sending checksum request to %s. If they cannot connect (checksum mismatch or cannot compare checksum) they are likely missing one of the following files:", entrant.Driver.Name)

	for _, file := range m.state.checkSummableFiles {
		m.logger.Infof(file.Filename)
		w.WriteString(file.Filename)
	}

	w.WriteString(strings.Join(m.state.raceConfig.LegalTyres, ";"))
	w.Write(m.state.randomSeed)
	w.Write(uint32(currentTimeMillisecond()))

	if err := w.WriteTCP(conn); err != nil {
		return err
	}

	entrant.Connection.HasSentFirstUpdate = false

	go func() {
		err := m.plugin.OnNewConnection(*entrant)

		if err != nil {
			m.logger.WithError(err).Error("On new connection plugin returned an error")
		}
	}()

	return nil
}

func (m HandshakeMessageHandler) MessageType() MessageType {
	return TCPHandshakeBegin
}
