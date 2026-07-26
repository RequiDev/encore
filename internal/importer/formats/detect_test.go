package formats

import (
	"testing"

	"github.com/requi/encore/internal/domain"
)

func TestDetectByName(t *testing.T) {
	cases := map[string]domain.ImportFormat{
		"Streaming_History_Audio_2018-2019_0.json":            domain.FormatExtended,
		"streaming_history_audio_2024_11.json":                domain.FormatExtended,
		"my_spotify_data/Streaming_History_Audio_2020_3.json": domain.FormatExtended,
		"Spotify Extended Streaming History\\endsong_7.json":  domain.FormatExtended,
		"endsong_0.json":                                 domain.FormatExtended,
		"endsong.json":                                   domain.FormatExtended,
		"Streaming_History_Audio_2021_1.json.gz":         domain.FormatExtended,
		"StreamingHistory0.json":                         domain.FormatAccountData,
		"StreamingHistory_music_4.json":                  domain.FormatAccountData,
		"MyData/streaminghistory_podcast_0.json":         domain.FormatAccountData,
		"Playlist1.json":                                 domain.FormatUnknown,
		"SearchQueries.json":                             domain.FormatUnknown,
		"Userdata.json":                                  domain.FormatUnknown,
		"YourLibrary.json":                               domain.FormatUnknown,
		"Marquee.json":                                   domain.FormatUnknown,
		"Read Me First.pdf":                              domain.FormatUnknown,
		"Read Me First - Extended Streaming History.pdf": domain.FormatUnknown,
		"notes.txt":                                      domain.FormatUnknown,
		"history.csv":                                    domain.FormatUnknown,
		"":                                               domain.FormatUnknown,
		"Streaming_History_Video_2023.json":              domain.FormatUnknown,
		"my_spotify_data/Follow.json":                    domain.FormatUnknown,
		"Spotify Account Data/Inferences.json":           domain.FormatUnknown,
		"my_spotify_data/Streaming_History_Audio_2020_3.json.part": domain.FormatUnknown,
	}
	for name, want := range cases {
		if got := DetectByName(name); got != want {
			t.Errorf("DetectByName(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestDetectByContent(t *testing.T) {
	cases := []struct {
		name string
		head string
		want domain.ImportFormat
	}{
		{
			name: "extended",
			head: `[{"ts":"2024-01-01T00:00:00Z","platform":"android","ms_played":1000,`,
			want: domain.FormatExtended,
		},
		{
			name: "extended without ms_played in the head",
			head: `[{"ts":"2024-01-01T00:00:00Z","master_metadata_track_name":"T"}]`,
			want: domain.FormatExtended,
		},
		{
			name: "account data",
			head: `[{"endTime":"2020-02-14 21:37","artistName":"A","trackName":"T","msPlayed":528000}]`,
			want: domain.FormatAccountData,
		},
		{
			name: "account data podcast variant",
			head: `[{"endTime":"2021-03-03 07:15","podcastName":"P","episodeName":"E","msPlayed":10}]`,
			want: domain.FormatAccountData,
		},
		{
			// Marquee.json carries an artistName but no duration; a looser test
			// would import a list of marketing segments as listening history.
			name: "marquee",
			head: `[{"artistName":"Nils Frahm","segment":"Super Listeners"}]`,
			want: domain.FormatUnknown,
		},
		{
			name: "playlists",
			head: `{"playlists":[{"name":"Chill","lastModifiedDate":"2023-01-02","items":[]}]}`,
			want: domain.FormatUnknown,
		},
		{
			name: "search queries",
			head: `[{"platform":"WEB","searchTime":"2023-01-01T00:00:00Z[UTC]","searchQuery":"frahm"}]`,
			want: domain.FormatUnknown,
		},
		{
			name: "a key that only appears as a value",
			head: `[{"field":"ms_played","value":"nonsense"}]`,
			want: domain.FormatUnknown,
		},
		{name: "empty", head: "", want: domain.FormatUnknown},
		{name: "not json", head: "%PDF-1.7 read me first", want: domain.FormatUnknown},
		{
			name: "leading bom and whitespace",
			head: string(utf8BOM) + "\n  " + `[ { "ts" : "2024-01-01T00:00:00Z" , "ms_played" : 10 } ]`,
			want: domain.FormatExtended,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectByContent([]byte(tc.head)); got != tc.want {
				t.Errorf("DetectByContent(%q) = %s, want %s", tc.head, got, tc.want)
			}
		})
	}
}

func TestDetectPrefersContentOverName(t *testing.T) {
	extended := readFixture(t, "extended_modern.json")
	accountData := readFixture(t, "accountdata.json")

	if got := Detect("my-listening-history.json", extended); got != domain.FormatExtended {
		t.Errorf("renamed extended file detected as %s", got)
	}
	if got := Detect("Streaming_History_Audio_0.json", accountData); got != domain.FormatAccountData {
		t.Errorf("mislabelled account data detected as %s", got)
	}
	// With no content to go on, the name is all there is.
	if got := Detect("endsong_0.json", nil); got != domain.FormatExtended {
		t.Errorf("name-only detection gave %s", got)
	}
	if got := Detect("Marquee.json", []byte(`[{"artistName":"A","segment":"S"}]`)); got != domain.FormatUnknown {
		t.Errorf("Marquee.json detected as %s", got)
	}
}
