package acserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	soloQualifyingIntroMessage = "This session is a Solo Qualifying session. You will not see any other cars on track for the duration of this session."
)

type SessionConfig struct {
	SessionType SessionType `json:"session_type" yaml:"session_type"`
	Name        string      `json:"name" yaml:"name"`
	Time        uint16      `json:"time" yaml:"time"`
	Laps        uint16      `json:"laps" yaml:"laps"`
	IsOpen      OpenRule    `json:"is_open" yaml:"is_open"`
	Solo        bool        `json:"solo" yaml:"solo"`
	WaitTime    int         `json:"wait_time" yaml:"wait_time"`
}

func (sc SessionConfig) IsSoloQualifying() bool {
	return sc.SessionType == SessionTypeQualifying && sc.Solo
}

type sessionParams struct {
	startTime, moveToNextSessionAt int64
	sessionOverBroadcast           bool
	reverseGridRaceStarted         bool
	numCompletedLaps               int
}

type CurrentSession struct {
	sessionParams
	Config SessionConfig

	mutex sync.RWMutex
}

func NewCurrentSession(config SessionConfig) CurrentSession {
	return CurrentSession{
		Config: config,
	}
}

func (s *CurrentSession) CompleteLap() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.numCompletedLaps++
}

func (s *CurrentSession) ResetLaps() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.numCompletedLaps = 0
}

func (s *CurrentSession) ClearData() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.sessionParams = sessionParams{}
}

func (s *CurrentSession) FinishTime() int64 {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if s.Config.Laps > 0 {
		logrus.Errorf("SessionConfig.FinishTime was called on a session which has laps.")
		return 0
	}

	return s.startTime + int64(s.Config.Time)*60*1000
}

func (s *CurrentSession) NumCompletedLaps() int {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	return s.numCompletedLaps
}

func (s *CurrentSession) String() string {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var sessionLength string

	if s.Config.Laps > 0 {
		sessionLength = fmt.Sprintf("%d Laps", s.Config.Laps)
	} else {
		sessionLength = fmt.Sprintf("%d minutes", s.Config.Time)
	}

	return fmt.Sprintf("%s - Name: %s, Length: %s, Wait Time: %ds, Open Rule: %s, Solo: %t", s.Config.SessionType, s.Config.Name, sessionLength, s.Config.WaitTime, s.Config.IsOpen, s.Config.Solo)
}

func (s *CurrentSession) IsZero() bool {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	return s.Config.SessionType == 0 && s.Config.Name == "" && s.Config.Time == 0 && s.Config.Laps == 0
}

type SessionManager struct {
	state                     *ServerState
	lobby                     *Lobby
	plugin                    Plugin
	logger                    Logger
	weatherManager            *WeatherManager
	dynamicTrack              *DynamicTrack
	serverStopFn              func(bool) error
	leaderboardAtSessionStart []*LeaderboardLine

	mutex               sync.RWMutex
	currentSessionIndex uint8
	currentSession      CurrentSession

	baseDirectory string
}

func NewSessionManager(state *ServerState, weatherManager *WeatherManager, lobby *Lobby, dynamicTrack *DynamicTrack, plugin Plugin, logger Logger, serverStopFn func(bool) error, baseDirectory string) *SessionManager {
	return &SessionManager{
		state:          state,
		lobby:          lobby,
		dynamicTrack:   dynamicTrack,
		weatherManager: weatherManager,
		serverStopFn:   serverStopFn,
		plugin:         plugin,
		logger:         logger,
		baseDirectory:  baseDirectory,
	}
}

func (sm *SessionManager) SaveResultsAndBuildLeaderboard(forceAdvance bool) (previousSessionLeaderboard []*LeaderboardLine, resultsFileName string) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()
	defer sm.ClearSessionData()

	if sm.currentSession.IsZero() {
		return
	}

	sm.logger.Infof("Leaderboard at the end of the session '%s' is:", sm.currentSession.Config.Name)

	previousSessionLeaderboard = sm.state.Leaderboard(sm.currentSession.Config.SessionType)

	for pos, leaderboardLine := range previousSessionLeaderboard {
		sm.logger.Printf("%d. %s - %s", pos, leaderboardLine.Car.Driver.Name, leaderboardLine)

		leaderboardLine.Car.GridPosition = pos + 1
	}

	if sm.currentSession.numCompletedLaps > 0 {
		results := sm.state.GenerateResults(sm.currentSession.Config)

		if err := saveResults(sm.baseDirectory, results); err != nil {
			sm.logger.WithError(err).Error("Could not save results file")
		} else {
			resultsFileName = results.SessionFile
		}

		if sm.state.raceConfig.ReversedGridRacePositions != 0 && !sm.currentSession.reverseGridRaceStarted && int(sm.currentSessionIndex) == len(sm.state.raceConfig.Sessions)-1 {
			// if there are reverse grid positions, then we need to reorganise the grid
			sm.logger.Infof("Next session is reverse grid race. Resetting session params, reverse grid is:")

			sm.currentSession.sessionParams = sessionParams{
				reverseGridRaceStarted: true,
			}

			ReverseLeaderboard(int(sm.state.raceConfig.ReversedGridRacePositions), previousSessionLeaderboard)

			for pos, leaderboardLine := range previousSessionLeaderboard {
				sm.logger.Printf("%d. %s - %s", pos, leaderboardLine.Car.Driver.Name, leaderboardLine)
			}
		} else {
			sm.currentSessionIndex++
		}
	} else {
		sm.logger.Infof("Session '%s' had no completed laps. Will not save results JSON", sm.currentSession.Config.Name)

		if forceAdvance {
			sm.currentSessionIndex++
		} else {
			switch sm.currentSession.Config.SessionType {
			case SessionTypeRace:
				sm.currentSessionIndex = 0
			case SessionTypeBooking:
				if len(sm.state.entryList) > 0 {
					sm.currentSessionIndex++
				}
			case SessionTypePractice:
				sm.currentSessionIndex++
			default:
				// current session index is left unchanged for qualifying.
			}
		}
	}

	return previousSessionLeaderboard, resultsFileName
}

func (sm *SessionManager) NextSession(force, wasRestart bool) {
	previousSessionLeaderboard, resultsFileName := sm.SaveResultsAndBuildLeaderboard(force)

	if resultsFileName != "" && !wasRestart {
		go func() {
			if err := sm.plugin.OnEndSession(resultsFileName); err != nil {
				sm.logger.WithError(err).Error("OnEndSession plugin errored")
			}
		}()
	}

	if int(sm.currentSessionIndex) >= len(sm.state.raceConfig.Sessions) {
		if sm.state.raceConfig.LoopMode {
			sm.logger.Infof("Loop Mode is enabled. Server will restart from first session.")
			sm.dynamicTrack.Init()

			sm.mutex.Lock()
			sm.currentSessionIndex = 0
			sm.mutex.Unlock()

			// mark the lobby as not registered so that the register request gets through
			sm.lobby.SetIsRegistered(false)

			if err := sm.lobby.Try("Register to lobby", sm.lobby.Register, true); err != nil {
				sm.logger.WithError(err).Error("All attempts to register to lobby failed")
			}
		} else {
			_ = sm.serverStopFn(false)
			return
		}
	}

	sm.mutex.Lock()
	sm.currentSession = NewCurrentSession(*sm.state.raceConfig.Sessions[sm.currentSessionIndex])
	sm.currentSession.startTime = sm.state.CurrentTimeMillisecond() + int64(sm.currentSession.Config.WaitTime*1000)
	sm.currentSession.moveToNextSessionAt = 0
	sm.currentSession.sessionOverBroadcast = false
	currentSessionIndex := sm.currentSessionIndex

	if !wasRestart {
		sm.logger.Infof("Advanced to next session: %s", sm.currentSession.String())
		sm.leaderboardAtSessionStart = previousSessionLeaderboard
	} else {
		sm.logger.Infof("Restarted current session: %s", sm.currentSession.String())
		previousSessionLeaderboard = sm.leaderboardAtSessionStart
	}
	sm.mutex.Unlock()

	for _, entrant := range sm.state.entryList {
		if entrant.IsConnected() {
			sm.SendSessionInfo(entrant, previousSessionLeaderboard)
		}
	}

	sm.BroadcastSessionStart()

	currentSessionConfig := sm.GetCurrentSession()

	sm.weatherManager.OnNewSession(currentSessionConfig)
	sm.dynamicTrack.OnNewSession(currentSessionConfig.SessionType)

	if currentSessionConfig.IsSoloQualifying() {
		sm.state.BroadcastChat(ServerCarID, soloQualifyingIntroMessage, false)

		go func() {
			// this code will only happen for looped servers and drivers where the driver loaded in to a race session
			// before the loop happened. any driver that loaded in for SessionTypePractice or SessionTypeQualifying will
			// never hit this code, and their 'load position' will be correctly set to their pit box.
			time.Sleep(time.Second)

			for _, car := range sm.state.entryList {
				if !car.IsConnected() || !car.HasSentFirstUpdate() {
					continue
				}

				if _, loadedInSessionType := car.GetCarLoadPosition(); loadedInSessionType != SessionTypeRace {
					continue
				}

				sm.logger.Infof("Solo Qualifying locked position guessed for Car: %s (pos: %s)", car.String(), car.PluginStatus.Position.String())
				car.SetCarLoadPosition(car.PluginStatus, SessionTypeQualifying)
			}
		}()
	}

	sm.UpdateLobby()

	go func() {
		err := sm.plugin.OnNewSession(sm.BuildSessionInfo(int(currentSessionIndex)))

		if err != nil {
			sm.logger.WithError(err).Error("On new session plugin returned an error")
		}
	}()
}

func (sm *SessionManager) BuildSessionInfo(sessionIndex int) SessionInfo {
	var currentSessionConfig SessionConfig

	if sessionIndex < 0 {
		sessionIndex = int(sm.GetSessionIndex())
		currentSessionConfig = sm.GetCurrentSession()
	} else {
		config, err := sm.GetSessionConfigForIndex(sessionIndex)

		if err != nil {
			return SessionInfo{}
		}

		currentSessionConfig = *config
	}

	return SessionInfo{
		Version:             uint8(CurrentProtocolVersion),
		SessionIndex:        uint8(sessionIndex),
		CurrentSessionIndex: sm.GetSessionIndex(),
		SessionCount:        uint8(len(sm.state.raceConfig.Sessions)),
		ServerName:          sm.state.serverConfig.Name,
		Track:               sm.state.raceConfig.Track,
		TrackConfig:         sm.state.raceConfig.TrackLayout,
		Name:                currentSessionConfig.Name,
		NumMinutes:          currentSessionConfig.Time,
		NumLaps:             currentSessionConfig.Laps,
		WaitTime:            currentSessionConfig.WaitTime,
		AmbientTemp:         sm.weatherManager.currentWeather.Ambient,
		RoadTemp:            sm.weatherManager.currentWeather.Road,
		WeatherGraphics:     sm.weatherManager.currentWeather.GraphicsName,
		ElapsedTime:         sm.ElapsedSessionTime(),
		SessionType:         currentSessionConfig.SessionType,
		IsSolo:              currentSessionConfig.Solo,
		CurrentGrip:         sm.dynamicTrack.CurrentGrip(),
	}
}

func (sm *SessionManager) loop(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	lastLobbyUpdate := time.Now()

	for {
		select {
		case <-ctx.Done():
			sm.logger.Debugf("Stopping SessionManager Loop")
			return
		case <-tick.C:
			if sm.state.serverConfig.RegisterToLobby && time.Since(lastLobbyUpdate) > time.Minute {
				sm.UpdateLobby()
				lastLobbyUpdate = time.Now()
			}

			if sm.CanBroadcastEndSession() {
				carsAreConnecting := false

				for _, car := range sm.state.entryList {
					if car.IsConnected() && !car.HasSentFirstUpdate() {
						carsAreConnecting = true
						sm.logger.Infof("Stalling end session until: %s has connected", car.String())
						break
					}
				}

				if carsAreConnecting {
					// don't advance sessions while cars are connecting.
					sm.logger.Infof("Stalling end session until all connecting cars are connected")
					continue
				}

				sm.BroadcastSessionCompleted()
			}

			if sm.CanMoveToNextSession() {
				carsAreConnecting := false

				for _, car := range sm.state.entryList {
					if car.IsConnected() && !car.HasSentFirstUpdate() {
						carsAreConnecting = true
						sm.logger.Infof("Stalling next session until: %s has connected", car.String())
						break
					}
				}

				if carsAreConnecting {
					// don't advance sessions while cars are connecting.
					sm.logger.Infof("Stalling next session until all connecting cars are connected")
					continue
				}

				// move to the next session when the race over packet has been sent and the results screen time has elapsed.
				sm.NextSession(false, false)
			}
		}
	}
}

func (sm *SessionManager) CanMoveToNextSession() bool {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()
	return sm.currentSession.sessionOverBroadcast && sm.state.CurrentTimeMillisecond() > sm.currentSession.moveToNextSessionAt
}

func (sm *SessionManager) CanBroadcastEndSession() bool {
	sm.mutex.RLock()
	moveToNextSessionAt := sm.currentSession.moveToNextSessionAt
	sessionOverBroadcast := sm.currentSession.sessionOverBroadcast
	sm.mutex.RUnlock()

	return moveToNextSessionAt == 0 && sm.CurrentSessionHasFinished() && !sessionOverBroadcast
}

func (sm *SessionManager) RestartSession() {
	sm.mutex.Lock()
	sm.currentSessionIndex--
	sm.mutex.Unlock()
	sm.NextSession(true, true)
}

func (sm *SessionManager) CurrentSessionHasFinished() bool {
	currentSessionConfig := sm.GetCurrentSession()

	switch currentSessionConfig.SessionType {
	case SessionTypeRace:
		var remainingLaps int
		var remainingTime time.Duration

		if currentSessionConfig.Laps > 0 {
			remainingLaps = sm.RemainingLaps()

			if remainingLaps > 0 {
				return false
			}
		} else {
			remainingTime = sm.RemainingSessionTime()

			if remainingTime >= 0 {
				return false
			}
		}

		if !sm.LeaderHasFinishedSession() {
			return false
		}

		if sm.AllCarsHaveFinishedSession() {
			sm.logger.Infof("All cars in session: %s have completed final laps. Ending session now.", currentSessionConfig.Name)
			return true
		}

		leaderboard := sm.state.Leaderboard(currentSessionConfig.SessionType)

		if len(leaderboard) == 0 {
			return true
		}

		leader := leaderboard[0].Car
		raceOverTime := time.Duration(int64(sm.state.raceConfig.RaceOverTime)*1000) * time.Millisecond

		leaderLaps := leader.GetLaps()

		return time.Since(leaderLaps[len(leaderLaps)-1].CompletedTime) > raceOverTime
	case SessionTypeBooking:
		return sm.RemainingSessionTime() <= 0
	default:
		remainingTime := sm.RemainingSessionTime()

		if remainingTime >= 0 {
			return false
		}

		if sm.AllCarsHaveFinishedSession() {
			sm.logger.Infof("All cars in session: %s have completed final laps. Ending session now.", currentSessionConfig.Name)
			return true
		}

		bestLapTime := sm.BestLapTimeInSession()

		if bestLapTime == maximumLapTime {
			// no laps were completed in this session
			sm.logger.Infof("Session: %s has no laps. Advancing to next session now.", currentSessionConfig.Name)
			return true
		}

		if currentSessionConfig.SessionType == SessionTypePractice && sm.AllRemainingCarsAreGoingSlow() {
			sm.logger.Infof("All cars in session: %s have completed final laps or are going slow. Ending session now.", currentSessionConfig.Name)
			return true
		}

		waitTime := time.Duration(float64(bestLapTime.Milliseconds())*float64(sm.state.raceConfig.QualifyMaxWaitPercentage)/100) * time.Millisecond

		return remainingTime < -waitTime
	}
}

func (sm *SessionManager) RemainingSessionTime() time.Duration {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	return time.Duration(sm.currentSession.FinishTime()-sm.state.CurrentTimeMillisecond()) * time.Millisecond
}

func (sm *SessionManager) RemainingLaps() int {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	leaderboard := sm.state.Leaderboard(sm.currentSession.Config.SessionType)
	numLapsInSession := int(sm.currentSession.Config.Laps)

	if len(leaderboard) == 0 {
		return numLapsInSession
	}

	remainingLaps := numLapsInSession - leaderboard[0].NumLaps

	return remainingLaps
}

func (sm *SessionManager) LeaderHasFinishedSession() bool {
	if sm.state.entryList.NumConnected() == 0 {
		return true
	}

	leaderHasCrossedLine := false

	for pos, leaderboardLine := range sm.state.Leaderboard(sm.currentSession.Config.SessionType) {
		if pos == 0 && leaderboardLine.Car.HasCompletedSession() {
			leaderHasCrossedLine = true
			break
		}
	}

	return leaderHasCrossedLine
}

func (sm *SessionManager) AllCarsHaveFinishedSession() bool {
	if sm.state.entryList.NumConnected() == 0 {
		return true
	}

	finished := true

	for _, entrant := range sm.state.entryList {
		finished = finished && (!entrant.IsConnected() || entrant.HasCompletedSession())
	}

	return finished
}

func (sm *SessionManager) AllRemainingCarsAreGoingSlow() bool {
	if sm.state.entryList.NumConnected() == 0 {
		return true
	}

	allGoingSlow := true

	for _, entrant := range sm.state.entryList {
		if !entrant.IsConnected() || entrant.HasCompletedSession() {
			continue
		}

		allGoingSlow = allGoingSlow && entrant.PluginStatus.Velocity.Magnitude() < 5
	}

	return allGoingSlow
}

func (sm *SessionManager) BestLapTimeInSession() time.Duration {
	var bestLapTime time.Duration

	for _, entrant := range sm.state.entryList {
		best := entrant.BestLap(sm.currentSession.Config.SessionType)

		if bestLapTime == 0 {
			bestLapTime = best.LapTime
		}

		if best.LapTime < bestLapTime {
			bestLapTime = best.LapTime
		}
	}

	return bestLapTime
}

func (sm *SessionManager) ElapsedSessionTime() time.Duration {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()
	return time.Duration(sm.state.CurrentTimeMillisecond()-sm.currentSession.startTime) * time.Millisecond
}

func (sm *SessionManager) ClearSessionData() {
	sm.logger.Infof("Clearing session data for all cars")
	sm.currentSession.ClearData()

	for _, car := range sm.state.entryList {
		car.ClearSessionData()
	}
}

func (sm *SessionManager) JoinIsAllowed(guid string) bool {
	if entrant := sm.state.GetCarByGUID(guid, false); entrant != nil {
		// entrants which were previously in this race are allowed back in
		if !entrant.Driver.LoadTime.IsZero() {
			return true
		}
	}

	currentSessionConfig := sm.GetCurrentSession()

	switch currentSessionConfig.IsOpen {
	case NoJoin:
		return false
	case FreeJoin:
		return true
	case FreeJoinUntil20SecondsBeforeGreenLight:
		return sm.ElapsedSessionTime() <= -20*time.Second
	default:
		return true
	}
}

func (sm *SessionManager) UpdateLobby() {
	if !sm.state.serverConfig.RegisterToLobby {
		return
	}

	updateFunc := func() error {
		remaining := 0

		currentSessionConfig := sm.GetCurrentSession()

		if currentSessionConfig.Laps > 0 {
			remaining = sm.RemainingLaps()
		} else {
			remaining = int(sm.RemainingSessionTime().Seconds())
		}

		return sm.lobby.UpdateSessionDetails(currentSessionConfig.SessionType, remaining)
	}

	if err := sm.lobby.Try("Update lobby with session details", updateFunc, false); err != nil {
		sm.logger.WithError(err).Error("All attempts to update lobby with session details failed")
	}
}

func (sm *SessionManager) GetCurrentSession() SessionConfig {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	return sm.currentSession.Config
}

func (sm *SessionManager) GetSessionConfigForIndex(i int) (*SessionConfig, error) {
	if i < 0 || i >= len(sm.state.raceConfig.Sessions) {
		return nil, errors.New("acserver: invalid session index specified")
	}

	return sm.state.raceConfig.Sessions[i], nil
}

func (sm *SessionManager) GetSessionIndex() uint8 {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	return sm.currentSessionIndex
}

func (sm *SessionManager) SetSessionIndex(i uint8) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	sm.currentSessionIndex = i
}

func (sm *SessionManager) SendSessionInfo(entrant *Car, leaderBoard []*LeaderboardLine) {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	if leaderBoard == nil {
		leaderBoard = sm.state.Leaderboard(sm.currentSession.Config.SessionType)
	}

	sm.logger.Debugf("Sending Client Session Information")

	bw := NewPacket(nil)
	bw.Write(TCPMessageCurrentSessionInfo)
	bw.WriteString(sm.currentSession.Config.Name)
	bw.Write(sm.currentSessionIndex)               // session index
	bw.Write(sm.currentSession.Config.SessionType) // type
	bw.Write(sm.currentSession.Config.Time)        // time
	bw.Write(sm.currentSession.Config.Laps)        // laps
	bw.Write(sm.dynamicTrack.CurrentGrip())        // dynamic track, grip

	for _, leaderboardLine := range leaderBoard {
		bw.Write(leaderboardLine.Car.CarID)
	}

	bw.Write(sm.currentSession.startTime - int64(entrant.Connection.TimeOffset))

	sm.state.WritePacket(bw, entrant.Connection.tcpConn)
}

func (sm *SessionManager) BroadcastSessionStart() {
	if sm.state.entryList.NumConnected() == 0 {
		return
	}

	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	sm.logger.Infof("Broadcasting Session Start packet")

	for _, entrant := range sm.state.entryList {
		if entrant.IsConnected() && entrant.HasSentFirstUpdate() {
			p := NewPacket(nil)
			p.Write(TCPMessageSessionStart)
			p.Write(int32(sm.currentSession.startTime - int64(entrant.Connection.TimeOffset)))
			p.Write(uint32(sm.state.CurrentTimeMillisecond() - int64(entrant.Connection.TimeOffset)))
			p.Write(uint16(entrant.Connection.Ping))

			sm.state.WritePacket(p, entrant.Connection.tcpConn)
		}
	}
}

func (sm *SessionManager) CompleteLap(carID CarID, lap *LapCompleted, target *Car) error {
	if carID != ServerCarID {
		sm.logger.Infof("CarID: %d just completed lap: %s (%d cuts) (splits: %v)", carID, time.Duration(lap.LapTime)*time.Millisecond, lap.Cuts, lap.Splits)
		sm.currentSession.CompleteLap()
		sm.dynamicTrack.OnLapCompleted()
	}

	car, err := sm.state.GetCarByID(carID)

	if err != nil {
		return err
	}

	if car.HasCompletedSession() {
		// entrants which have completed the session can't complete more laps
		return nil
	}

	l := car.AddLap(lap, sm.state.CurrentTimeMillisecond(), sm.currentSession.startTime)

	if carID != ServerCarID {
		// last sector only
		var cutsInSectorsSoFar uint8

		for _, sector := range car.SessionData.Sectors {
			cutsInSectorsSoFar += sector.Cuts
		}

		cutsInFinalSector := lap.Cuts - cutsInSectorsSoFar

		split := Split{
			Index: lap.NumSplits - 1,
			Time:  lap.Splits[lap.NumSplits-1],
			Cuts:  cutsInFinalSector,
		}

		go func() {
			err = sm.plugin.OnSectorCompleted(car.Copy(), split)

			if err != nil {
				sm.logger.WithError(err).Error("On sector completed plugin returned an error")
			}

			err := sm.plugin.OnLapCompleted(car.CarID, *l)

			if err != nil {
				sm.logger.WithError(err).Error("On lap completed plugin returned an error")
			}
		}()
	}

	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	leaderboard := sm.state.Leaderboard(sm.currentSession.Config.SessionType)

	if sm.currentSession.Config.Laps > 0 {
		car.SetHasCompletedSession(car.LapCount() == int(sm.currentSession.Config.Laps))
	} else {
		if sm.state.CurrentTimeMillisecond() > sm.currentSession.FinishTime() {
			leader := leaderboard[0]

			if sm.state.raceConfig.RaceExtraLap {
				if car.SessionData.HasExtraLapToGo {
					// everyone at this point has completed their extra lap
					car.SetHasCompletedSession(true)
				} else {
					// the entrant has another lap to go if they are the leader, or the leader has an extra lap to go
					car.SessionData.HasExtraLapToGo = leader.Car == car || leader.Car.SessionData.HasExtraLapToGo
				}
			} else {
				// the entrant has completed the session if they are the leader or the leader has completed the session.
				car.SetHasCompletedSession(leader.Car == car || leader.Car.HasCompletedSession())
			}
		}
	}

	bw := NewPacket(nil)
	bw.Write(TCPMessageLapCompleted)
	bw.Write(carID)
	bw.Write(lap.LapTime)
	bw.Write(lap.Cuts)
	bw.Write(uint8(len(sm.state.entryList)))

	for _, leaderBoardLine := range leaderboard {
		bw.Write(leaderBoardLine.Car.CarID)

		switch sm.currentSession.Config.SessionType {
		case SessionTypeRace:
			bw.Write(uint32(leaderBoardLine.Time.Milliseconds()))
		default:
			bw.Write(uint32(leaderBoardLine.Time.Milliseconds()))
		}

		bw.Write(uint16(leaderBoardLine.NumLaps))
		bw.Write(leaderBoardLine.Car.HasCompletedSession())
	}

	bw.Write(sm.state.dynamicTrack.CurrentGrip())

	if target != nil {
		sm.state.WritePacket(bw, target.Connection.tcpConn)
		return nil
	}

	sm.state.BroadcastAllTCP(bw)

	return nil
}

func (sm *SessionManager) BroadcastSessionCompleted() {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	switch sm.currentSession.Config.SessionType {
	case SessionTypeBooking:
		sm.currentSession.moveToNextSessionAt = sm.state.CurrentTimeMillisecond()
	default:
		sm.currentSession.moveToNextSessionAt = sm.state.CurrentTimeMillisecond() + int64(sm.state.raceConfig.ResultScreenTime*1000)
	}

	sm.currentSession.sessionOverBroadcast = true

	sm.logger.Infof("Broadcasting session completed packet for session: %s", sm.currentSession.Config.SessionType)
	p := NewPacket(nil)
	p.Write(TCPMessageSessionCompleted)

	for _, leaderboardLine := range sm.state.Leaderboard(sm.currentSession.Config.SessionType) {
		p.Write(leaderboardLine.Car.CarID)
		p.Write(uint32(leaderboardLine.Time.Milliseconds()))
		p.Write(uint16(leaderboardLine.NumLaps))
	}

	// this bool here was previously used by Kunos to indicate to kick all users out post-session if loop mode was on
	// i'd like us not to require this if at all possible, so hopefully we can ignore it for now and just return '1'
	// (i.e. car can stay in server as sessions cycle)
	p.Write(uint8(1))

	sm.state.BroadcastAllTCP(p)
}

func ReverseLeaderboard(numToReverse int, leaderboard []*LeaderboardLine) {
	if numToReverse == 0 {
		return
	}

	if numToReverse > len(leaderboard) || numToReverse < 0 {
		numToReverse = len(leaderboard)
	}

	for i, line := range leaderboard {
		if i > numToReverse {
			break
		}

		if !line.Car.HasCompletedSession() {
			numToReverse = i
			break
		}
	}

	toReverse := leaderboard[:numToReverse]

	for i := len(toReverse)/2 - 1; i >= 0; i-- {
		opp := len(toReverse) - 1 - i
		toReverse[i], toReverse[opp] = toReverse[opp], toReverse[i]
	}

	for i := 0; i < len(toReverse); i++ {
		leaderboard[i] = toReverse[i]
	}
}