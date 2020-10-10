package acserver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

var serverStartTime = time.Now()

func currentTimeMillisecond() int64 {
	return time.Since(serverStartTime).Milliseconds()
}

type SessionConfig struct {
	SessionType SessionType `json:"session_type" yaml:"session_type"`
	Name        string      `json:"name" yaml:"name"`
	Time        uint16      `json:"time" yaml:"time"`
	Laps        uint16      `json:"laps" yaml:"laps"`
	IsOpen      OpenRule    `json:"is_open" yaml:"is_open"`
	Solo        bool        `json:"solo" yaml:"solo"`
	WaitTime    int         `json:"wait_time" yaml:"wait_time"`
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
	state          *ServerState
	lobby          *Lobby
	plugin         Plugin
	logger         Logger
	weatherManager *WeatherManager
	dynamicTrack   *DynamicTrack
	serverStopFn   func() error

	mutex               sync.RWMutex
	currentSessionIndex uint8
	currentSession      CurrentSession

	baseDirectory string
}

func NewSessionManager(state *ServerState, weatherManager *WeatherManager, lobby *Lobby, dynamicTrack *DynamicTrack, plugin Plugin, logger Logger, serverStopFn func() error, baseDirectory string) *SessionManager {
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

func (sm *SessionManager) SaveResultsAndBuildLeaderboard(forceAdvance bool) []*LeaderboardLine {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var previousSessionLeaderboard []*LeaderboardLine

	if !sm.currentSession.IsZero() {
		if sm.currentSession.numCompletedLaps > 0 {
			sm.logger.Infof("Leaderboard at the end of the session '%s' is:", sm.currentSession.Config.Name)

			previousSessionLeaderboard = sm.state.Leaderboard(sm.currentSession.Config.SessionType)

			for pos, leaderboardLine := range previousSessionLeaderboard {
				sm.logger.Printf("%d. %s - %s", pos, leaderboardLine.Car.Driver.Name, leaderboardLine)
			}

			results := sm.state.GenerateResults(sm.currentSession.Config)
			err := saveResults(sm.baseDirectory, results)

			if err != nil {
				sm.logger.WithError(err).Error("Could not save results file!")
			} else {
				err := sm.plugin.OnEndSession(results.SessionFile)

				if err != nil {
					sm.logger.WithError(err).Error("On end session plugin returned an error")
				}
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

		sm.ClearSessionData()
	}

	return previousSessionLeaderboard
}

func (sm *SessionManager) NextSession(force bool) {
	previousSessionLeaderboard := sm.SaveResultsAndBuildLeaderboard(force)

	if int(sm.currentSessionIndex) >= len(sm.state.raceConfig.Sessions) {
		if sm.state.raceConfig.LoopMode {
			sm.logger.Infof("Loop Mode is enabled. Server will restart from first session.")
			sm.dynamicTrack.Init()

			sm.mutex.Lock()
			sm.currentSessionIndex = 0
			sm.mutex.Unlock()
		} else {
			_ = sm.serverStopFn()
			return
		}
	}

	sm.mutex.Lock()
	sm.currentSession = NewCurrentSession(*sm.state.raceConfig.Sessions[sm.currentSessionIndex])
	sm.currentSession.startTime = currentTimeMillisecond() + int64(sm.currentSession.Config.WaitTime*1000)
	sm.currentSession.moveToNextSessionAt = 0
	sm.currentSession.sessionOverBroadcast = false
	currentSessionIndex := sm.currentSessionIndex
	sm.mutex.Unlock()

	sm.logger.Infof("Advanced to next session: %s", sm.currentSession.String())

	for _, entrant := range sm.state.entryList {
		if entrant.IsConnected() {
			if err := sm.SendSessionInfo(entrant, previousSessionLeaderboard); err != nil {
				sm.logger.WithError(err).Error("Couldn't send session info")
			}
		}
	}

	sm.BroadcastSessionStart(sm.currentSession.startTime)

	sm.weatherManager.OnNewSession(sm.currentSession.Config)
	sm.dynamicTrack.OnNewSession(sm.currentSession.Config.SessionType)

	sm.UpdateLobby()

	err := sm.plugin.OnNewSession(SessionInfo{
		Version:         CurrentResultsVersion,
		SessionIndex:    currentSessionIndex,
		SessionCount:    uint8(len(sm.state.raceConfig.Sessions)),
		ServerName:      sm.state.serverConfig.Name,
		Track:           sm.state.raceConfig.Track,
		TrackConfig:     sm.state.raceConfig.TrackLayout,
		Name:            sm.currentSession.Config.Name,
		NumMinutes:      sm.currentSession.Config.Time,
		NumLaps:         sm.currentSession.Config.Laps,
		WaitTime:        sm.currentSession.Config.WaitTime,
		AmbientTemp:     sm.weatherManager.currentWeather.Ambient,
		RoadTemp:        sm.weatherManager.currentWeather.Road,
		WeatherGraphics: sm.weatherManager.currentWeather.GraphicsName,
		ElapsedTime:     sm.ElapsedSessionTime(),
		SessionType:     sm.currentSession.Config.SessionType,
		IsSolo:          sm.currentSession.Config.Solo,
	})

	if err != nil {
		sm.logger.WithError(err).Error("On new session plugin returned an error")
	}
}

func (sm *SessionManager) loop(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			sm.logger.Debugf("Stopping SessionManager Loop")
			return
		case <-tick.C:
			if sm.CanBroadcastEndSession() {
				carsAreConnecting := false

				for _, car := range sm.state.entryList {
					if car.IsConnected() && !car.HasSentFirstUpdate() {
						carsAreConnecting = true
						break
					}
				}

				if carsAreConnecting {
					// don't advance sessions while cars are connecting.
					sm.logger.Debugf("Stalling session until all connecting cars are connected")
					continue
				}

				sm.BroadcastSessionCompleted()

				sm.mutex.Lock()
				switch sm.currentSession.Config.SessionType {
				case SessionTypeBooking:
					sm.currentSession.moveToNextSessionAt = currentTimeMillisecond()
				default:
					sm.currentSession.moveToNextSessionAt = currentTimeMillisecond() + int64(sm.state.raceConfig.ResultScreenTime*1000)
				}
				sm.currentSession.sessionOverBroadcast = true
				sm.mutex.Unlock()
			}

			if sm.CanMoveToNextSession() {
				carsAreConnecting := false

				for _, car := range sm.state.entryList {
					if car.IsConnected() && !car.HasSentFirstUpdate() {
						carsAreConnecting = true
						break
					}
				}

				if carsAreConnecting {
					// don't advance sessions while cars are connecting.
					sm.logger.Debugf("Stalling session until all connecting cars are connected")
					continue
				}

				// move to the next session when the race over packet has been sent and the results screen time has elapsed.
				sm.NextSession(false)
			}
		}
	}
}

func (sm *SessionManager) CanMoveToNextSession() bool {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()
	return sm.currentSession.sessionOverBroadcast && currentTimeMillisecond() > sm.currentSession.moveToNextSessionAt
}

func (sm *SessionManager) CanBroadcastEndSession() bool {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	return sm.currentSession.moveToNextSessionAt == 0 && sm.CurrentSessionHasFinished() && !sm.currentSession.sessionOverBroadcast
}

func (sm *SessionManager) RestartSession() {
	sm.mutex.Lock()
	sm.currentSessionIndex--
	sm.mutex.Unlock()
	sm.NextSession(true)
}

func (sm *SessionManager) CurrentSessionHasFinished() bool {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	switch sm.currentSession.Config.SessionType {
	case SessionTypeRace:
		var remainingLaps int
		var remainingTime time.Duration

		if sm.currentSession.Config.Laps > 0 {
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
			sm.logger.Infof("All cars in session: %s have completed final laps. Ending session now.", sm.currentSession.Config.Name)
			return true
		}

		leaderboard := sm.state.Leaderboard(sm.currentSession.Config.SessionType)

		if len(leaderboard) == 0 {
			return true
		}

		leader := leaderboard[0].Car
		raceOverTime := time.Duration(int64(sm.state.raceConfig.RaceOverTime)*1000) * time.Millisecond

		return time.Since(leader.SessionData.Laps[leader.SessionData.LapCount-1].CompletedTime) > raceOverTime
	case SessionTypeBooking:
		return sm.RemainingSessionTime() <= 0
	default:
		remainingTime := sm.RemainingSessionTime()

		if remainingTime >= 0 {
			return false
		}

		if sm.AllCarsHaveFinishedSession() {
			sm.logger.Infof("All cars in session: %s have completed final laps. Ending session now.", sm.currentSession.Config.Name)
			return true
		}

		bestLapTime := sm.BestLapTimeInSession()

		if bestLapTime == maximumLapTime {
			// no laps were completed in this session
			sm.logger.Infof("Session: %s has no laps. Advancing to next session now.", sm.currentSession.Config.Name)
			return true
		}

		waitTime := time.Duration(float64(bestLapTime.Milliseconds())*float64(sm.state.raceConfig.QualifyMaxWaitPercentage)/100) * time.Millisecond

		return remainingTime < -waitTime
	}
}

func (sm *SessionManager) RemainingSessionTime() time.Duration {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	return time.Duration(sm.currentSession.FinishTime()-currentTimeMillisecond()) * time.Millisecond
}

func (sm *SessionManager) RemainingLaps() int {
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
		if pos == 0 && leaderboardLine.Car.SessionData.HasCompletedSession {
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
		finished = finished && (!entrant.IsConnected() || entrant.SessionData.HasCompletedSession)
	}

	return finished
}

func (sm *SessionManager) BestLapTimeInSession() time.Duration {
	var bestLapTime time.Duration

	for _, entrant := range sm.state.entryList {
		best := entrant.BestLap()

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
	return time.Duration(currentTimeMillisecond()-sm.currentSession.startTime) * time.Millisecond
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

	switch sm.currentSession.Config.IsOpen {
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

		if sm.currentSession.Config.Laps > 0 {
			remaining = sm.RemainingLaps()
		} else {
			remaining = int(sm.RemainingSessionTime().Seconds())
		}

		return sm.lobby.UpdateSessionDetails(sm.currentSession.Config.SessionType, remaining)
	}

	if err := sm.lobby.Try("Update lobby with new session", updateFunc); err != nil {
		sm.logger.WithError(err).Error("All attempts to update lobby with new session failed")
	}
}

func (sm *SessionManager) GetCurrentSession() SessionConfig {
	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	return sm.currentSession.Config
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

func (sm *SessionManager) SendSessionInfo(entrant *Car, leaderBoard []*LeaderboardLine) error {
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

	return bw.WriteTCP(entrant.Connection.tcpConn)
}

func (sm *SessionManager) BroadcastSessionStart(startTime int64) {
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
			p.Write(uint32(startTime - int64(entrant.Connection.TimeOffset)))
			p.Write(uint16(entrant.Connection.Ping))

			if err := p.WriteTCP(entrant.Connection.tcpConn); err != nil {
				sm.logger.WithError(err).Errorf("Could not send race start packet to %s", entrant.String())
			}
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

	if car.SessionData.HasCompletedSession {
		// entrants which have completed the session can't complete more laps
		return nil
	}

	l := car.AddLap(lap)

	if carID != ServerCarID {
		err := sm.plugin.OnLapCompleted(car.CarID, *l)

		if err != nil {
			sm.logger.WithError(err).Error("On lap completed plugin returned an error")
		}

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

		err = sm.plugin.OnSectorCompleted(car.Copy(), split)

		if err != nil {
			sm.logger.WithError(err).Error("On sector completed plugin returned an error")
		}
	}

	sm.mutex.RLock()
	defer sm.mutex.RUnlock()

	leaderboard := sm.state.Leaderboard(sm.currentSession.Config.SessionType)

	if sm.currentSession.Config.Laps > 0 {
		car.SessionData.HasCompletedSession = car.SessionData.LapCount == int(sm.currentSession.Config.Laps)
	} else {
		if currentTimeMillisecond() > sm.currentSession.FinishTime() {
			leader := leaderboard[0]

			if sm.state.raceConfig.RaceExtraLap {
				if car.SessionData.HasExtraLapToGo {
					// everyone at this point has completed their extra lap
					car.SessionData.HasCompletedSession = true
				} else {
					// the entrant has another lap to go if they are the leader, or the leader has an extra lap to go
					car.SessionData.HasExtraLapToGo = leader.Car == car || leader.Car.SessionData.HasExtraLapToGo
				}
			} else {
				// the entrant has completed the session if they are the leader or the leader has completed the session.
				car.SessionData.HasCompletedSession = leader.Car == car || leader.Car.SessionData.HasCompletedSession
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
		bw.Write(leaderBoardLine.Car.SessionData.HasCompletedSession)
	}

	bw.Write(sm.state.dynamicTrack.CurrentGrip())

	if target != nil {
		return bw.WriteTCP(target.Connection.tcpConn)
	}

	sm.state.BroadcastAllTCP(bw)

	return nil
}

func (sm *SessionManager) BroadcastSessionCompleted() {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

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

		if !line.Car.SessionData.HasCompletedSession {
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
