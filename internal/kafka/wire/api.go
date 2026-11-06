package wire

// API keys served by the Kafka server.
const (
	APIProduce          = 0
	APIFetch            = 1
	APIListOffsets      = 2
	APIMetadata         = 3
	APIOffsetCommit     = 8
	APIOffsetFetch      = 9
	APIFindCoordinator  = 10
	APIJoinGroup        = 11
	APIHeartbeat        = 12
	APILeaveGroup       = 13
	APISyncGroup        = 14
	APISaslHandshake    = 17
	APIApiVersions      = 18
	APIDescribeConfigs  = 32
	APISaslAuthenticate = 36
)

// Kafka error codes used by the server.
const (
	ErrNone                   int16 = 0
	ErrUnknownServerError     int16 = -1
	ErrUnknownTopicOrPart     int16 = 3
	ErrKafkaStorageError      int16 = 56
	ErrUnsupportedVersion     int16 = 35
	ErrTopicAuthFailed        int16 = 29
	ErrUnsupportedSASLMech    int16 = 33
	ErrSASLAuthFailed         int16 = 58
	ErrCorruptMessage         int16 = 2
	ErrUnsupportedCompression int16 = 76
)

// APIRange is the version range advertised for one API key.
type APIRange struct{ Key, Min, Max int16 }

// SupportedAPIs is the ApiVersions response of the main broker's Kafka server.
var SupportedAPIs = []APIRange{
	{APIProduce, 0, 7},
	{APIFetch, 0, 10},
	{APIListOffsets, 0, 3},
	{APIMetadata, 0, 4},
	{APIOffsetCommit, 0, 7},
	{APIOffsetFetch, 0, 1},
	{APIFindCoordinator, 0, 0},
	{APIJoinGroup, 0, 2},
	{APIHeartbeat, 0, 2},
	{APILeaveGroup, 0, 2},
	{APISyncGroup, 0, 2},
	{APISaslHandshake, 0, 1},
	{APIApiVersions, 0, 0},
	{APISaslAuthenticate, 0, 1},
	{APIDescribeConfigs, 0, 1},
}

// SupportedRange returns the range for key, ok=false when unknown.
func SupportedRange(key int16) (APIRange, bool) {
	for _, a := range SupportedAPIs {
		if a.Key == key {
			return a, true
		}
	}
	return APIRange{}, false
}
