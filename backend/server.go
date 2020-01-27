/*
 * Implements the RPC server for yt_box backend
 */

package backend

import (
	"database/sql"
	"io"
	"io/ioutil"
	"log"
	"net"
	"sync"

	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	queuer "github.com/nguyenmq/ytbox-go/backend/song_queuer"
	db "github.com/nguyenmq/ytbox-go/database"
	bepb "github.com/nguyenmq/ytbox-go/proto/backend"
	cmpb "github.com/nguyenmq/ytbox-go/proto/common"
)

const (
	LogPrefix     string = "ytb-be"           // logging prefix name
	queueSnapshot string = "/tmp/ytbox.queue" // location of the queue snapshot
)

/*
 * Implements the backend rpc server interface
 */
type BackendServer struct {
	listener  net.Listener             // network listener
	beServer  *grpc.Server             // backend RPC server
	queueMgr  *queuer.SongQueueManager // playlist queue
	dbManager db.DbManager             // database manager
	userCache *UserCache               // user identity cache
	playerMgr *playerManager           // player manager
	streamWG  sync.WaitGroup           // wait group for streaming goroutines
}

/*
 * Create a new yt_box backend server
 */
func NewServer(addr string, loadFile string, dbPath string) *BackendServer {
	var err error

	// initialize the backend server struct
	server := new(BackendServer)
	server.listener, err = net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s with error: %v", addr, err)
	}

	// initialize the rpc server
	server.beServer = grpc.NewServer()
	bepb.RegisterYtbBackendServer(server.beServer, server)
	bepb.RegisterYtbBePlayerServer(server.beServer, server)

	// initialize the song queue
	server.queueMgr = new(queuer.SongQueueManager)
	server.queueMgr.Init(queuer.NewRoundRobinQueuer())

	// initialize the database manager
	server.dbManager = new(db.SqliteManager)
	server.dbManager.Init(dbPath)

	// initialize the user identity cache
	server.userCache = new(UserCache)
	server.userCache.Init()

	// load a snapshot playlist if provided
	if loadFile != "" {
		server.loadPlaylistFromFile(loadFile)
	}

	// initialize the player manager
	server.playerMgr = new(playerManager)
	server.playerMgr.init(server.queueMgr)

	return server
}

/*
 * Start the server
 */
func (s *BackendServer) Serve() {
	s.playerMgr.start()
	s.beServer.Serve(s.listener)
}

/*
 * Stop the server
 */
func (s *BackendServer) Stop() {
	// stop the player manager
	s.playerMgr.stop()

	// wait for all the rpc streaming connections to close
	s.streamWG.Wait()

	// stop the rpc server
	s.beServer.GracefulStop()
}

/*
 * Receive a song from a remote client for appending to the play queue
 */
func (s *BackendServer) SendSong(con context.Context, sub *bepb.Submission) (*bepb.Error, error) {
	response := &bepb.Error{Success: false}
	log.Printf("Submission: {link: %s, userId: %d}\n", sub.Link, sub.UserId)

	song := new(cmpb.Song)
	song.UserId = sub.GetUserId()

	song.Username = s.getUsernameFromId(song.UserId)
	if song.Username == "" {
		response.Message = "Song submitted by unknown user"
		log.Printf(response.Message)
		return response, nil
	}

	err := fetchSongData(sub.Link, song)
	if err != nil {
		response.Message = err.Error()
		log.Println(err.Error())
		return response, nil
	}

	response.Success = true
	response.Message = "Success"
	s.queueMgr.AddSong(song)
	s.dbManager.AddSong(song)
	s.queueMgr.SavePlaylist(queueSnapshot)
	log.Printf("Song data: { %v}", song)

	return response, nil
}

/*
 * Load a playlist from a serialized protobuf file
 */
func (s *BackendServer) loadPlaylistFromFile(file string) {
	in, err := ioutil.ReadFile(file)
	if err != nil {
		log.Printf("Error reading file: %s", file)
		return
	}

	playlist := &bepb.Playlist{}
	err = proto.Unmarshal(in, playlist)
	if err != nil {
		log.Printf("Failed to parse playlist file: %v", err)
		return
	}

	log.Printf("Loading songs from file \"%s\":", file)
	for i := 0; i < len(playlist.Songs); i++ {
		song := &cmpb.Song{
			Title:     playlist.Songs[i].Title,
			SongId:    playlist.Songs[i].SongId,
			Username:  playlist.Songs[i].Username,
			UserId:    playlist.Songs[i].UserId,
			Service:   playlist.Songs[i].Service,
			ServiceId: playlist.Songs[i].ServiceId,
		}
		s.queueMgr.AddSong(song)
		log.Printf("%3d. { %v}", i+1, song)
	}
}

/*
 * Returns the songs in the queue back to the requesting client
 */
func (s *BackendServer) GetPlaylist(con context.Context, arg *cmpb.Empty) (*bepb.Playlist, error) {
	return s.queueMgr.GetPlaylist(), nil
}

/*
 * Login the given user. If the userId is zero, then a new user needs to be
 * created. A successful call will echo the username and return a userId
 * greater than zero. An id of zero indicates an error occurred and the user
 * will not be considered to be logged in. An a user already exists with the
 * given id, but with a different name, then the new name shall be applied to
 * the database.
 */
func (s *BackendServer) LoginUser(con context.Context, user *bepb.User) (*bepb.User, error) {
	userData, err := s.dbManager.GetUserById(user.UserId)

	if userData == nil {
		// Something happened if user data is nil

		if err == sql.ErrNoRows {
			// if no results were returned, then create a new user
			userData, err = s.dbManager.AddUser(user.Username)
			if err != nil {
				log.Printf("Failed to add user: %s", user.Username)
				return &bepb.User{Username: user.Username, UserId: 0}, nil
			}
		} else {
			// else return an error to the rpc client
			return &bepb.User{Username: user.Username, UserId: 0}, nil
		}
	} else if userData.User.Username != user.Username {
		// Update the username in the database if the names differ
		err = s.dbManager.UpdateUsername(user.Username, user.UserId)
		if err != nil {
			log.Println("Could not update username")
			return &bepb.User{Username: user.Username, UserId: 0}, nil
		}
	}

	// cache the user id and username
	s.userCache.AddUserToCache(userData.User.UserId, user.Username)

	return &bepb.User{Username: user.Username, UserId: userData.User.UserId}, nil
}

/*
 * Pops a song off the top of the queueMgr and returns it
 */
func (s *BackendServer) PopQueue(con context.Context, empty *cmpb.Empty) (*cmpb.Song, error) {
	if s.queueMgr.Len() > 0 {
		song := s.queueMgr.PopQueue()
		s.queueMgr.SavePlaylist(queueSnapshot)
		log.Printf("Popped song: { %v}", song)
		return song, nil
	}

	log.Println("Queue is empty, nothing to pop")
	return &cmpb.Song{}, nil
}

/*
 * Saves the current playlist to the given file location
 */
func (s *BackendServer) SavePlaylist(con context.Context, fname *bepb.FilePath) (*bepb.Error, error) {
	response := &bepb.Error{Success: false}
	err := s.queueMgr.SavePlaylist(fname.Path)
	if err != nil {
		response.Message = err.Error()
		return response, nil
	}

	log.Printf("Saved current playlist to: %s", fname.Path)
	response.Success = true
	response.Message = "Success"
	return response, nil
}

/*
 * Returns the username associated with the user id. An empty string is
 * returned if there was an error or the user id wasn't found.
 */
func (s *BackendServer) getUsernameFromId(userId uint32) string {
	if userId == 0 {
		log.Println("User id of zero was passed into getUsernameFromId")
		return ""
	}

	// check the user identities cache for the name
	username, exists := s.userCache.LookupUsername(userId)
	if exists {
		return username
	}

	// retrieve the name from the database if the user isn't in the cache
	userData, err := s.dbManager.GetUserById(userId)
	if err != nil {
		log.Printf("Failed to get username from database with id: %d", userId)
		return ""
	}

	// Add the username to the cache and return the name we found in the
	// database
	s.userCache.AddUserToCache(userId, userData.User.Username)
	return userData.User.Username
}

/*
 * Removes the given song from the playlist. The user identified by the song
 * eviction must match the id of the user who submitted the song.
 */
func (s *BackendServer) RemoveSong(con context.Context, eviction *bepb.Eviction) (*bepb.Error, error) {
	err := s.queueMgr.RemoveSong(eviction.GetSongId(), eviction.GetUserId())

	if err != nil {
		log.Printf("Failed to remove song from playlist: %v", err)
		return &bepb.Error{Success: false, Message: err.Error()}, nil
	} else {
		log.Printf("Removed song: {song id: %d, user id: %d}", eviction.GetSongId(), eviction.GetUserId())
		return &bepb.Error{Success: true, Message: "Success"}, nil
	}
}

/*
 * Returns the song that should be considered "now playing". If there isn't a
 * current song, then an empty Song struct is returned.
 */
func (s *BackendServer) GetNowPlaying(con context.Context, empty *cmpb.Empty) (*cmpb.Song, error) {
	nowPlaying := s.queueMgr.NowPlaying()

	if nowPlaying == nil {
		return &cmpb.Song{}, nil
	}

	return nowPlaying, nil
}

/*
 * Forwards the command to skip the currently playing song onto the remote
 * player
 */
func (s *BackendServer) NextSong(con context.Context, empty *cmpb.Empty) (*bepb.Error, error) {
	nextSong := s.queueMgr.PopQueue()
	control := &bepb.PlayerControl{Command: bepb.CommandType_Next, Song: nextSong}
	s.playerMgr.sendToPlayers(control)
	return &bepb.Error{Success: true, Message: "Success"}, nil
}

/*
 * Forwards the command to pause the currently playing song onto the remote
 * player
 */
func (s *BackendServer) PauseSong(con context.Context, empty *cmpb.Empty) (*bepb.Error, error) {
	s.playerMgr.sendToPlayers(&bepb.PlayerControl{Command: bepb.CommandType_Pause})
	return &bepb.Error{Success: true, Message: "Success"}, nil
}

/*
 * Stream RPC connection with the remote player client
 */
func (s *BackendServer) SongPlayer(stream bepb.YtbBePlayer_SongPlayerServer) error {
	s.streamWG.Add(1)
	defer s.streamWG.Done()
	id, stop := s.playerMgr.add(stream)

	go func() {
		for {
			status, err := stream.Recv()
			if err == io.EOF {
				log.Printf("Disconnected from remote player")
				break
			}

			if grpc.Code(err) == codes.Canceled {
				break
			}

			if err != nil {
				log.Printf("Error receiving message from remote player: %v", err)
				break
			}

			// write the received status to the player manager
			s.playerMgr.receiveFromPlayers(id, status)
		}
		stop <- struct{}{}
	}()

	<-stop
	s.playerMgr.remove(id)
	return nil
}
