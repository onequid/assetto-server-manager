package acserver

import (
	"net"
	"time"
)

type PositionMessageHandler struct {
	state          *ServerState
	sessionManager *SessionManager
	weatherManager *WeatherManager
	plugin         Plugin
	logger         Logger
}

func NewPositionMessageHandler(state *ServerState, sessionManager *SessionManager, weatherManager *WeatherManager, plugin Plugin, logger Logger) *PositionMessageHandler {
	return &PositionMessageHandler{
		state:          state,
		sessionManager: sessionManager,
		weatherManager: weatherManager,
		plugin:         plugin,
		logger:         logger,
	}
}

const (
	HeadlightByte = 0b100000
	DRSByte       = 0b10000000000
)

type CarUpdate struct {
	Sequence            uint8
	Timestamp           uint32
	Position            Vector3F
	Rotation            Vector3F
	Velocity            Vector3F
	TyreAngularSpeed    [4]uint8
	SteerAngle          uint8
	WheelAngle          uint8
	EngineRPM           uint16
	GearIndex           uint8
	StatusBytes         uint32
	PerformanceDelta    int16
	Gas                 uint8
	NormalisedSplinePos float32
}

func (pm *PositionMessageHandler) OnMessage(_ net.PacketConn, addr net.Addr, p *Packet) error {
	var carUpdate CarUpdate

	p.Read(&carUpdate)

	car := pm.state.GetCarByUDPAddress(addr)

	if car == nil {
		return nil
	}

	if pm.state.raceConfig.ForceOpponentHeadlights {
		carUpdate.StatusBytes |= HeadlightByte
	}

	if car.HasSentFirstUpdate() && carUpdate.Timestamp < car.PluginStatus.Timestamp {
		pm.logger.Warnf("Position packet out of order for %s previous: %d received: %d", car.Driver.Name, car.PluginStatus.Timestamp, carUpdate.Timestamp)

		return nil
	}

	car.SetPluginStatus(carUpdate)

	currentSession := pm.sessionManager.GetCurrentSession()

	if currentSession.IsSoloQualifying() {
		pitboxPosition, _ := car.GetCarLoadPosition()
		pitboxPosition.Timestamp = carUpdate.Timestamp
		pitboxPosition.Sequence = carUpdate.Sequence

		car.SetStatus(pitboxPosition)
	} else {
		car.SetStatus(carUpdate)
	}

	car.SetHasUpdateToBroadcast(true)
	car.AdjustTimeOffset()

	if !car.HasSentFirstUpdate() {
		if car.HasFailedChecksum() {
			if err := pm.state.Kick(car.CarID, KickReasonChecksumFailed); err != nil {
				return err
			}
		}

		car.SetHasSentFirstUpdate(true)
		car.SetCarLoadPosition(carUpdate, currentSession.SessionType)

		if err := pm.SendFirstUpdate(car); err != nil {
			return err
		}

		go func() {
			err := pm.plugin.OnClientLoaded(car.Copy())

			if err != nil {
				pm.logger.WithError(err).Error("On client loaded plugin returned an error")
			}
		}()
	}

	return nil
}

func (pm *PositionMessageHandler) SendFirstUpdate(car *Car) error {
	pm.logger.Infof("Sending first update to client: %s", car.String())

	bw := NewPacket(nil)
	bw.Write(TCPConnectedEntrants)
	bw.Write(uint8(len(pm.state.entryList)))

	for _, entrant := range pm.state.entryList {
		bw.Write(entrant.CarID)
		bw.WriteUTF32String(entrant.Driver.Name)
	}

	pm.state.WritePacket(bw, car.Connection.tcpConn)

	// send weather to car
	pm.weatherManager.SendWeather(car)

	// send a lap completed message for car ID 0xFF to broadcast all other lap times to the connecting user.
	if err := pm.sessionManager.CompleteLap(ServerCarID, &LapCompleted{}, car); err != nil {
		return err
	}

	for _, otherEntrant := range pm.state.entryList {
		if car.CarID == otherEntrant.CarID {
			continue
		}

		bw := NewPacket(nil)
		bw.Write(TCPMessageTyreChange)
		bw.Write(otherEntrant.CarID)
		bw.WriteString(otherEntrant.Tyres)

		pm.state.WritePacket(bw, car.Connection.tcpConn)

		bw = NewPacket(nil)

		bw.Write(TCPMessagePushToPass)
		bw.Write(otherEntrant.CarID)
		bw.Write(otherEntrant.SessionData.P2PCount)
		bw.Write(uint8(0))

		pm.state.WritePacket(bw, car.Connection.tcpConn)

		bw = NewPacket(nil)
		bw.Write(TCPMandatoryPitCompleted)
		bw.Write(otherEntrant.CarID)

		if otherEntrant.SessionData.MandatoryPit {
			bw.Write(uint8(0x01))
		} else {
			bw.Write(uint8(0x00))
		}

		pm.state.WritePacket(bw, car.Connection.tcpConn)
	}

	car.SetLoadedTime(time.Now())

	// send bop for car
	pm.state.SendBoP(car)

	// send MOTD to the newly connected car
	pm.state.SendMOTD(car)

	// send fixed setup too
	pm.state.SendSetup(car)

	// if there are drs zones, send them too
	pm.state.SendDRSZones(car)

	currentSession := pm.sessionManager.GetCurrentSession()

	if currentSession.IsSoloQualifying() {
		if err := pm.state.SendChat(ServerCarID, car.CarID, soloQualifyingIntroMessage, false); err != nil {
			pm.logger.WithError(err).Errorf("Couldn't send solo qualifying intro message")
		}
	}

	return nil
}

func (pm *PositionMessageHandler) MessageType() MessageType {
	return UDPMessageCarUpdate
}