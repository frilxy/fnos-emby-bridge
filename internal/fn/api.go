package fn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// flexStr 兼容飞牛字段在 string / []string 间漂移（mediadb posters 是数组，item posters 是字符串）。
type flexStr string

func (f *flexStr) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		if len(arr) > 0 {
			*f = flexStr(arr[0])
		}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*f = flexStr(s)
	return nil
}

// MediaDB 对应 GET /v/api/v1/mediadb/list 的库条目。
type MediaDB struct {
	Guid     string  `json:"guid"`
	Title    string  `json:"title"`
	Posters  flexStr `json:"posters"`
	Poster   flexStr `json:"poster"`
	Category string  `json:"category"` // TV / Movie / Other / Live
	ViewType int     `json:"view_type"`
}

// MediaItem 对应 item/list / season/list / item/{guid} 的条目（仅留桥接所需字段）。
type MediaItem struct {
	Guid             string  `json:"guid"`
	ParentGuid       string  `json:"parent_guid"`
	AncestorGuid     string  `json:"ancestor_guid"`
	AncestorName     string  `json:"ancestor_name"`
	AncestorCategory string  `json:"ancestor_category"`
	Title            string  `json:"title"`
	TvTitle          string  `json:"tv_title"`
	ParentTitle      string  `json:"parent_title"`
	Type             string  `json:"type"` // MediaDB/TV/Season/Episode/Movie/Video/Directory
	Poster           flexStr `json:"poster"`
	Posters          flexStr `json:"posters"`
	PosterWidth      int     `json:"poster_width"`
	PosterHeight     int     `json:"poster_height"`
	Runtime          int     `json:"runtime"`  // 分钟
	Duration         int     `json:"duration"` // 秒
	IsFavorite       int     `json:"is_favorite"`
	Watched          int     `json:"watched"`
	IsWatched        int     `json:"is_watched"`
	WatchedTs        int64   `json:"watched_ts"`
	Ts               int64   `json:"ts"` // 续播位置（秒，真机确认）
	VoteAverage      string  `json:"vote_average"`
	Status           string  `json:"status"` // TV: "Returning Series"/"Ended"；其余 "1"
	SeasonNumber     int     `json:"season_number"`
	EpisodeNumber    int     `json:"episode_number"`
	NumberOfSeasons  int     `json:"number_of_seasons"`
	NumberOfEpisodes int     `json:"number_of_episodes"`
	// LocalNumberOfEpisodes 本地已有集数（季条目真机返回，episode_number 亦为本季集数）
	LocalNumberOfEpisodes int    `json:"local_number_of_episodes"`
	AirDate               string `json:"air_date"`
	ReleaseDate           string `json:"release_date"`
	FirstAirDate          string `json:"first_air_date"`
	Overview              string `json:"overview"`
	CanPlay               int    `json:"can_play"`
	FileName              string `json:"file_name"`
	// Logo/剧照（0.9.8 真机契约：剧条目带 logos/backdrops 横竖版资源，
	// 集 poster 即 1920×1080 横版剧照）
	Logos     flexStr `json:"logos"`
	Backdrops flexStr `json:"backdrops"`
	TrimID    string  `json:"trim_id"` // 飞牛 TMDB 刮削 ID
	ImdbID    string  `json:"imdb_id"`
}

// StreamFile 对应 stream/list 返回的 files 元素：同一集的多媒体版本。
type StreamFile struct {
	Guid       string `json:"guid"`
	Path       string `json:"path"`
	FileName   string `json:"file_name"`
	Size       int64  `json:"size"`
	CanPlay    int    `json:"can_play"`
	CreateTime int64  `json:"create_time"`
}

// StreamListStream 对应 stream/list 返回的 video_streams / audio_streams 元素。
type StreamListStream struct {
	MediaGuid      string `json:"media_guid"`
	Guid           string `json:"guid"`
	Title          string `json:"title"`
	ResolutionType string `json:"resolution_type"`
	CodecName      string `json:"codec_name"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	Profile        string `json:"profile"`
	Bps            int64  `json:"bps"`
	BitDepth       int    `json:"bit_depth"`
	PixelFmt       string `json:"pix_fmt"`
	Aspect         string `json:"display_aspect_ratio"`
	FrameRate      string `json:"r_frame_rate"`
	Language       string `json:"language"`
	Channels       int    `json:"channels"`
	ChannelLayout  string `json:"channel_layout"`
	SampleRate     string `json:"sample_rate"`
	Index          int    `json:"index"`
	IsDefault      int    `json:"is_default"`
	ColorRangeType string `json:"color_range_type"`
	Duration       int    `json:"duration"`
}

// PosterPath 取海报路径（poster 优先，回退 posters）。
func (m MediaItem) PosterPath() string {
	if m.Poster != "" {
		return string(m.Poster)
	}
	return string(m.Posters)
}

// LogoPath 取 Logo 图路径（剧条目专属，透明底标题字）。
func (m MediaItem) LogoPath() string { return string(m.Logos) }

// BackdropPath 取横版剧照路径（剧条目专属；集的 poster 本身即横版）。
func (m MediaItem) BackdropPath() string { return string(m.Backdrops) }

// PlayInfo 对应 POST /v/api/v1/play/info 响应（真机验证）。
type PlayInfo struct {
	Guid         string `json:"guid"`
	ParentGuid   string `json:"parent_guid"`
	GrandGuid    string `json:"grand_guid"`
	Type         string `json:"type"` // Episode / Movie / Video
	MediaGuid    string `json:"media_guid"`
	VideoGuid    string `json:"video_guid"`
	AudioGuid    string `json:"audio_guid"`
	SubtitleGuid string `json:"subtitle_guid"`
	Ts           int64  `json:"ts"` // 续播秒
	Item         struct {
		Guid        string  `json:"guid"`
		Title       string  `json:"title"`
		TvTitle     string  `json:"tv_title"`
		ParentTitle string  `json:"parent_title"`
		Posters     flexStr `json:"posters"`
		Poster      flexStr `json:"poster"`
		Runtime     int     `json:"runtime"`  // 分钟
		Duration    int     `json:"duration"` // 秒（部分版本返回）
		IsWatched   int     `json:"is_watched"`
		WatchedTs   int64   `json:"watched_ts"`
		SeasonNum   int     `json:"season_number"`
		EpisodeNum  int     `json:"episode_number"`
		AirDate     string  `json:"air_date"`
		Overview    string  `json:"overview"`
		CanPlay     int     `json:"can_play"`
		Type        string  `json:"type"`
	} `json:"item"`
}

// StreamList 对应 GET /v/api/v1/stream/list/{guid}：同一条目全部可播版本。
type StreamList struct {
	Files           []StreamFile       `json:"files"`
	VideoStreams    []StreamListStream `json:"video_streams"`
	AudioStreams    []StreamListStream `json:"audio_streams"`
	SubtitleStreams []SubtitleStream   `json:"subtitle_streams"`
}

// StreamResp 对应 POST /v/api/v1/stream 响应（真机验证 0.9.8）。
type StreamResp struct {
	FileStream struct {
		Guid     string `json:"guid"`
		Path     string `json:"path"` // NAS 真实文件路径（/vol02/...）——官方媒体信息顶部显示它
		FileName string `json:"file_name"`
		Size     int64  `json:"size"`
		CanPlay  int    `json:"can_play"`
		// 入库时间（毫秒时间戳）——Emby DateCreated 的真实来源
		// （此前用首播日期兜底，客户端媒体信息显示 "2025/07/08 00:00" 假时间）
		CreateTime int64 `json:"create_time"`
	} `json:"file_stream"`
	VideoStream *struct {
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Duration  int    `json:"duration"` // 秒（真机验证 0.9.8 返回）
		// 流详情（对照官方 Emby 媒体信息面板字段；真机 0.9.8 全部返回）
		Profile        string `json:"profile"`              // "Main 10"
		Bps            int64  `json:"bps"`                  // 码率 9882704
		BitDepth       int    `json:"bit_depth"`            // 10
		PixelFmt       string `json:"pix_fmt"`              // yuv420p10le
		Aspect         string `json:"display_aspect_ratio"` // "16:9"
		FrameRate      string `json:"r_frame_rate"`         // "23.98 fps"（带单位）
		ColorRangeType string `json:"color_range_type"`     // SDR / HDR（官方 VideoRange）
		ResolutionType string `json:"resolution_type"`      // 4k / 1080
	} `json:"video_stream"`
	AudioStreams []json.RawMessage `json:"audio_streams"`
	// 字幕流（真机 0.9.8：内嵌 ass 轨也在其中，is_external=0）
	SubtitleStreams  []SubtitleStream `json:"subtitle_streams"`
	CloudStorageInfo *struct {
		CloudStorageType int  `json:"cloud_storage_type"`
		Valid            bool `json:"valid"`
		Disabled         bool `json:"disabled"`
		IsVip            bool `json:"is_vip"`
	} `json:"cloud_storage_info"`
	Header struct {
		Cookie    []string `json:"Cookie"`
		UserAgent []string `json:"User-Agent"`
	} `json:"header"`
	DirectLinkQualities []struct {
		URL        string `json:"url"`
		Bitrate    int    `json:"bitrate"`
		Resolution string `json:"resolution"`
		// is_m3u8 老版本字段，0.9.8 直链无此字段，缺省 false
		IsM3u8 bool `json:"is_m3u8"`
	} `json:"direct_link_qualities"`
}

// SubtitleStream 对应 stream 响应 subtitle_streams 元素（真机 0.9.8）。
type SubtitleStream struct {
	MediaGuid  string `json:"media_guid"`
	Guid       string `json:"guid"`
	Title      string `json:"title"`
	CodecName  string `json:"codec_name"` // ass / srt / subrip ...
	Language   string `json:"language"`
	Index      int    `json:"index"`
	IsDefault  int    `json:"is_default"`
	IsExternal int    `json:"is_external"` // 0=内嵌容器 1=外挂文件
	IsBitmap   int    `json:"is_bitmap"`   // 1=图形字幕（PGS/VobSub）
	Format     string `json:"format"`
}

// Login 调 POST /v/api/v2/user/loginByPassword（0.9.8 真机契约）。
// passwordHash 必须已是 SHA256 hex（由调用方完成哈希——客户端可能发明文也可能
// 发已哈希密码，桥接层负责归一化，这里只透传，避免二次哈希）。
func (c *Client) Login(ctx context.Context, username, passwordHash string) error {
	res, err := c.doPath(ctx, "POST", "/v/api/v2/user/loginByPassword", map[string]interface{}{
		"app_name": appName,
		"username": username,
		"password": passwordHash,
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return fmt.Errorf("login decode token: %w", err)
	}
	if out.Token == "" {
		return fmt.Errorf("login: empty token")
	}
	c.SetToken(out.Token)
	return nil
}

// MediaDBList 调 GET /v/api/v1/mediadb/list，返回全部媒体库（0.9.8 支持直接列库，无需 SEED_GUIDS）。
func (c *Client) MediaDBList(ctx context.Context) ([]MediaDB, error) {
	res, err := c.do(ctx, "GET", "mediadb/list", nil)
	if err != nil {
		return nil, fmt.Errorf("mediadb/list: %w", err)
	}
	var out []MediaDB
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("mediadb/list decode: %w", err)
	}
	return out, nil
}

// ItemListPage 调 POST /v/api/v1/item/list（0.9.8 真机契约：分页 + tags.type 过滤）。
// mode 为 "ancestor_guid"（库级浏览）或 "parent_guid"（剧下钻季/季下钻集）——
// 真机验证两种模式互斥：库 guid 只认 ancestor，剧/季 guid 只认 parent。
func (c *Client) ItemListPage(ctx context.Context, mode, guid string, page, pageSize int) ([]MediaItem, int, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	body := map[string]interface{}{
		"tags":                  map[string]interface{}{"type": []string{"Movie", "TV", "Directory", "Video", "Season", "Episode"}},
		"sort_type":             "DESC",
		"sort_column":           "create_time",
		"exclude_grouped_video": 1,
		"page":                  page,
		"page_size":             pageSize,
		mode:                    guid,
	}
	res, err := c.do(ctx, "POST", "item/list", body)
	if err != nil {
		return nil, 0, fmt.Errorf("item/list %q: %w", guid, err)
	}
	var out struct {
		List  []MediaItem `json:"list"`
		Total int         `json:"total"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, 0, fmt.Errorf("item/list decode: %w", err)
	}
	return out.List, out.Total, nil
}

// ItemList 取 guid 下全部条目（自动模式适配）：
// 先试 ancestor_guid（库 guid 命中），返回 0 条则回退 parent_guid（剧/季 guid 命中）。
func (c *Client) ItemList(ctx context.Context, guid string) ([]MediaItem, error) {
	list, err := c.listAllPages(ctx, "ancestor_guid", guid)
	if err != nil {
		return nil, err
	}
	if len(list) > 0 {
		return list, nil
	}
	return c.listAllPages(ctx, "parent_guid", guid)
}

// ItemListContainers 只列容器级条目（Movie/TV/Directory/Video，不含集/季）。
// Emby Latest 语义是"最新添加的剧/电影"，服务端类型过滤避免大库时前 N 页全是集。
func (c *Client) ItemListContainers(ctx context.Context, guid string, limit int) ([]MediaItem, error) {
	return c.ItemListTyped(ctx, guid, []string{"Movie", "TV", "Directory", "Video"}, limit)
}

// ItemListTyped 按指定 tags.type 全分页拉取库 guid 下条目（ancestor 模式）。
// 供继续观看聚合用（Episode/Movie/Video 级筛 ts>0）。
func (c *Client) ItemListTyped(ctx context.Context, guid string, types []string, limit int) ([]MediaItem, error) {
	var all []MediaItem
	for page := 1; ; page++ {
		body := map[string]interface{}{
			"tags":                  map[string]interface{}{"type": types},
			"sort_type":             "DESC",
			"sort_column":           "create_time",
			"exclude_grouped_video": 1,
			"page":                  page,
			"page_size":             100,
			"ancestor_guid":         guid,
		}
		res, err := c.do(ctx, "POST", "item/list", body)
		if err != nil {
			return nil, fmt.Errorf("item/list typed %q: %w", guid, err)
		}
		var out struct {
			List  []MediaItem `json:"list"`
			Total int         `json:"total"`
		}
		if err := json.Unmarshal(res.Data, &out); err != nil {
			return nil, fmt.Errorf("item/list typed decode: %w", err)
		}
		all = append(all, out.List...)
		// 上限保护：防超大库无限翻页（继续观看场景 500 条足够）
		if (limit > 0 && len(all) >= limit) || len(all) >= out.Total || len(out.List) == 0 || len(all) >= 500 {
			break
		}
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// listAllPages 翻完全部页。
func (c *Client) listAllPages(ctx context.Context, mode, guid string) ([]MediaItem, error) {
	var all []MediaItem
	for page := 1; ; page++ {
		list, total, err := c.ItemListPage(ctx, mode, guid, page, 100)
		if err != nil {
			return nil, err
		}
		all = append(all, list...)
		if len(all) >= total || len(list) == 0 {
			break
		}
	}
	return all, nil
}

// ItemDetail 调 GET /v/api/v1/item/{guid} 取单条目详情。
func (c *Client) ItemDetail(ctx context.Context, guid string) (*MediaItem, error) {
	res, err := c.do(ctx, "GET", "item/"+guid, nil)
	if err != nil {
		return nil, fmt.Errorf("item %q: %w", guid, err)
	}
	var out MediaItem
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("item decode: %w", err)
	}
	return &out, nil
}

// SeasonList 调 GET /v/api/v1/season/list/{tvGuid} 取剧集的季列表。
func (c *Client) SeasonList(ctx context.Context, tvGuid string) ([]MediaItem, error) {
	res, err := c.do(ctx, "GET", "season/list/"+tvGuid, nil)
	if err != nil {
		return nil, fmt.Errorf("season/list %q: %w", tvGuid, err)
	}
	var out []MediaItem
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("season/list decode: %w", err)
	}
	return out, nil
}

// Person 对应 POST /v/api/v1/person/list/{guid} 的演职员条目（真机抓包 1.12.0）。
type Person struct {
	PersonGuid   string `json:"person_guid"`
	Role         string `json:"role"`          // 扮演角色名（"上野"）
	Job          string `json:"job"`           // Actor / Director / Writer ...
	Order        int    `json:"order"`         // 排序（1 起）
	Name         string `json:"name"`          // 译名（芹泽优）
	OriginalName string `json:"original_name"` // 原名（芹澤優）
	Biography    string `json:"biography"`
	ProfilePath  string `json:"profile_path"` // 头像路径（sys/img 资源）
	Gender       int    `json:"gender"`
}

// PersonList 调 POST /v/api/v1/person/list/{guid} 取条目演员表。
// item/{guid} 详情不带演职员（飞牛拆成独立端点），剧/电影 guid 有效。
func (c *Client) PersonList(ctx context.Context, guid string) ([]Person, error) {
	res, err := c.do(ctx, "POST", "person/list/"+guid, map[string]interface{}{
		"guid": guid, "page": 1, "page_size": 1000, "lan": "zh-CN",
	})
	if err != nil {
		return nil, fmt.Errorf("person/list %q: %w", guid, err)
	}
	var out struct {
		Total int      `json:"total"`
		List  []Person `json:"list"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("person/list decode: %w", err)
	}
	return out.List, nil
}

// EpisodeList 调 GET /v/api/v1/episode/list/{guid}——剧 guid 一次返回全剧所有集
// （跨季平铺，条目自带 ts/duration/watched/runtime，真机抓包验证），
// 比"剧→逐季 item/list"少 N 次请求。响应 data 为数组。
func (c *Client) EpisodeList(ctx context.Context, guid string) ([]MediaItem, error) {
	res, err := c.do(ctx, "GET", "episode/list/"+guid+"?guid="+guid+"&lan=zh-CN", nil)
	if err != nil {
		return nil, fmt.Errorf("episode/list %q: %w", guid, err)
	}
	var out []MediaItem
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("episode/list decode: %w", err)
	}
	return out, nil
}

// PersonDetail 对应 GET /v/api/v1/person/{guid}（真机抓包）：演员/声优详情。
type PersonDetail struct {
	Guid         string `json:"guid"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name"`
	Profile      string `json:"profile"` // 头像 sys/img 路径
	Biography    string `json:"biography"`
	IsFavorite   int    `json:"is_favorite"`
}

// PersonDetail 取演员详情（详情页点演员进人物页用）。
func (c *Client) PersonDetail(ctx context.Context, guid string) (*PersonDetail, error) {
	res, err := c.do(ctx, "GET", "person/"+guid, nil)
	if err != nil {
		return nil, fmt.Errorf("person %q: %w", guid, err)
	}
	var out PersonDetail
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("person decode: %w", err)
	}
	return &out, nil
}

// PersonItemList 调 POST /v/api/v1/person/item/list——某演员参演的作品列表
// （人物页"作品"网格）。job 传空查全部（Actor/Director/Writer...）。
func (c *Client) PersonItemList(ctx context.Context, personGuid, job string, page, pageSize int) ([]MediaItem, int, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	body := map[string]interface{}{
		"person_guid": personGuid,
		"page":        page,
		"page_size":   pageSize,
		"sort_column": "update_time",
		"sort_type":   "desc",
		"lan":         "zh-CN",
	}
	if job != "" {
		body["job"] = job
	}
	res, err := c.do(ctx, "POST", "person/item/list", body)
	if err != nil {
		return nil, 0, fmt.Errorf("person/item/list %q: %w", personGuid, err)
	}
	var out struct {
		Total int         `json:"total"`
		List  []MediaItem `json:"list"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, 0, fmt.Errorf("person/item/list decode: %w", err)
	}
	return out.List, out.Total, nil
}

// SetFavorite 收藏/取消收藏（真机抓包：PUT 收藏、DELETE 取消，body 只有 item_guid）。
func (c *Client) SetFavorite(ctx context.Context, itemGuid string, on bool) error {
	method := "PUT"
	if !on {
		method = "DELETE"
	}
	_, err := c.do(ctx, method, "item/favorite", map[string]interface{}{
		"item_guid": itemGuid,
		"lan":       "zh-CN",
	})
	if err != nil {
		return fmt.Errorf("item/favorite %q: %w", itemGuid, err)
	}
	return nil
}

// FavoriteList 调 POST /v/api/v1/favorite/list——收藏列表（分页，MediaItem 形态）。
// data.list 可能为 null（空收藏）。
func (c *Client) FavoriteList(ctx context.Context, page, pageSize int) ([]MediaItem, int, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	res, err := c.do(ctx, "POST", "favorite/list", map[string]interface{}{
		"exclude_grouped_video": 1,
		"sort_type":             "DESC",
		"sort_column":           "create_time",
		"page":                  page,
		"page_size":             pageSize,
		"tags":                  map[string]interface{}{"type": []string{}},
		"lan":                   "zh-CN",
	})
	if err != nil {
		return nil, 0, fmt.Errorf("favorite/list: %w", err)
	}
	var out struct {
		Total int         `json:"total"`
		List  []MediaItem `json:"list"`
	}
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, 0, fmt.Errorf("favorite/list decode: %w", err)
	}
	if out.List == nil {
		out.List = []MediaItem{}
	}
	return out.List, out.Total, nil
}

// RefreshItem 调 POST /v/api/v1/item/refresh 触发条目元数据刷新（refresh_mode=0）。
func (c *Client) RefreshItem(ctx context.Context, itemGuid string) error {
	_, err := c.do(ctx, "POST", "item/refresh", map[string]interface{}{
		"item_guid":    itemGuid,
		"refresh_mode": 0,
		"lan":          "zh-CN",
	})
	if err != nil {
		return fmt.Errorf("item/refresh %q: %w", itemGuid, err)
	}
	return nil
}

// PlayInfoByGuid 调 POST /v/api/v1/play/info（item_guid 可传 Movie/Episode，也可传 TV guid
// ——真机验证：对 TV guid 调用返回该季下一集）。
func (c *Client) PlayInfoByGuid(ctx context.Context, itemGuid string) (*PlayInfo, error) {
	res, err := c.do(ctx, "POST", "play/info", map[string]interface{}{
		"item_guid": itemGuid,
	})
	if err != nil {
		return nil, fmt.Errorf("play/info %q: %w", itemGuid, err)
	}
	var out PlayInfo
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("play/info decode: %w", err)
	}
	return &out, nil
}

// StreamListByGuid 调 GET /v/api/v1/stream/list/{guid}。
// guid 支持集、季和剧；返回中所有 media_guid 关联 files 即多版本。
func (c *Client) StreamListByGuid(ctx context.Context, guid string) (*StreamList, error) {
	res, err := c.do(ctx, "GET", "stream/list/"+guid+"", nil)
	if err != nil {
		return nil, fmt.Errorf("stream/list %q: %w", guid, err)
	}
	var out StreamList
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("stream/list decode: %w", err)
	}
	if out.Files == nil {
		out.Files = []StreamFile{}
	}
	return &out, nil
}

// FnPlayerUA 飞牛影视官方播放器 UA（真机抓包 1.12.0，com.trim.media Flutter 客户端）。
// 真机结论：夸克网盘对官方播放器 UA 的直链不限速，其它 UA（trim_player 等）会被限速
// ——取云盘直链必须带这个 UA 请求 stream，且后续直链请求沿用响应里的同名 UA。
const FnPlayerUA = "1.12.0 (com.trim.media; build:1120009; Android 16) Flutter/3.11.5"

// Stream 调 POST /v/api/v1/stream 获取播放流信息（含云盘直链）。
// ua 传空用官方播放器 UA（夸克直链不限速的前提）；ip 为 32 位 hex 设备指纹，level=1。
func (c *Client) Stream(ctx context.Context, mediaGuid, ua string) (*StreamResp, error) {
	if ua == "" {
		ua = FnPlayerUA
	}
	res, err := c.do(ctx, "POST", "stream", map[string]interface{}{
		"media_guid": mediaGuid,
		"ip":         "16a9d43e38a5f992b3f1c44d68940523", // 固定设备指纹（真机格式；直链未绑定请求 IP）
		"level":      1,
		"header":     map[string]interface{}{"User-Agent": []string{ua}},
	})
	if err != nil {
		return nil, fmt.Errorf("stream %q: %w", mediaGuid, err)
	}
	var out StreamResp
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("stream decode: %w", err)
	}
	return &out, nil
}

// RawStream 以自定义 JSON body 调 stream（限速参数矩阵实验用）。
func (c *Client) RawStream(ctx context.Context, rawBody string) (*StreamResp, error) {
	res, err := c.doRaw(ctx, "POST", "/v/api/v1/stream", rawBody)
	if err != nil {
		return nil, err
	}
	var out StreamResp
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("stream decode: %w", err)
	}
	return &out, nil
}

// RawAny 任意方法+路径+JSON body 的通用调用（接口探测用）。
func (c *Client) RawAny(ctx context.Context, method, fullPath string, body any) error {
	var raw string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	_, err := c.doRawBody(ctx, method, fullPath, raw)
	return err
}

// ExtractDirectLink 从 StreamResp 提取云盘直链信息。
// cloud_storage_info 为 null（本地 NAS 文件）时 CloudType=0，调用方走 RangeMedia。
func (c *Client) ExtractDirectLink(s *StreamResp) *DirectLinkInfo {
	info := &DirectLinkInfo{}
	if s == nil {
		return info
	}
	if s.CloudStorageInfo != nil {
		info.CloudType = s.CloudStorageInfo.CloudStorageType
		info.Cookies = s.Header.Cookie
	}
	for _, ua := range s.Header.UserAgent {
		if ua != "" {
			info.UserAgent = []string{ua}
			break
		}
	}
	if len(info.UserAgent) == 0 {
		info.UserAgent = []string{FnPlayerUA}
	}
	if len(s.DirectLinkQualities) > 0 {
		info.URL = s.DirectLinkQualities[0].URL
		info.IsM3u8 = s.DirectLinkQualities[0].IsM3u8
	}
	return info
}

// DirectLinkInfo 云盘直链所需信息。
type DirectLinkInfo struct {
	URL       string
	Cookies   []string
	UserAgent []string
	IsM3u8    bool
	// CloudType 云存储类型：1 百度 2 阿里 3 115 4 夸克 5 123，0=本地 NAS
	CloudType int
}

// RangeMedia 直接 GET /v/api/v1/media/range/{mediaGuid}?direct_link_quality_index=1，
// 带 authx 签名（真机验证：不带 authx 会被反代兜底成 SPA HTML），透传 Range 头。
func (c *Client) RangeMedia(ctx context.Context, mediaGuid string, rangeHdr string) (http.Header, int, io.ReadCloser, error) {
	path := "/v/api/v1/media/range/" + mediaGuid
	url := c.BaseURL + path + "?direct_link_quality_index=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("range req: %w", err)
	}
	req.Header.Set("Authx", authx(path, ""))
	req.Header.Set("User-Agent", c.UserAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", c.Token)
		req.Header.Set("Cookie", "Trim-MC-token="+c.Token+"; mode=relay")
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("range do: %w", err)
	}
	// 200 完整或 206 部分合法；其余（如 302→SPA HTML）视为失败
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 120))
		resp.Body.Close()
		return nil, 0, nil, fmt.Errorf("range status=%d body=%s", resp.StatusCode, string(b))
	}
	return resp.Header, resp.StatusCode, resp.Body, nil
}

// RecordProgress 调 POST /v/api/v1/play/record 回传播放进度（秒）。
func (c *Client) RecordProgress(ctx context.Context, itemGuid, mediaGuid string, ts, duration int64) error {
	_, err := c.do(ctx, "POST", "play/record", map[string]interface{}{
		"item_guid":  itemGuid,
		"media_guid": mediaGuid,
		"ts":         ts,
		"duration":   duration,
	})
	if err != nil {
		return fmt.Errorf("play/record: %w", err)
	}
	return nil
}

// WatchItem 调 POST /v/api/v1/item/watched 标记已看完。
// 真机抓包：飞牛播放器播完自动调用，body 只要 item_guid。
func (c *Client) WatchItem(ctx context.Context, itemGuid string) error {
	if _, err := c.do(ctx, "POST", "item/watched", map[string]interface{}{
		"item_guid": itemGuid,
		"lan":       "zh-CN",
	}); err != nil {
		return fmt.Errorf("item/watched: %w", err)
	}
	return nil
}

// UnwatchItem 取消已看（APK 逆向 + 实测）：DELETE /v/api/v1/item/watched
// + JSON body {item_guid}——与标记已看同端点异方法，DELETE 必须带 body。
// 实测矩阵（上野 S1E1）：POST watched=false / is_watched=0 / PUT watched=0
// 均无效（一律标已看），仅 DELETE+body 真正回退（is_watched 1→0、watched_ts 清零）。
func (c *Client) UnwatchItem(ctx context.Context, itemGuid string) error {
	if _, err := c.do(ctx, "DELETE", "item/watched", map[string]interface{}{
		"item_guid": itemGuid,
		"lan":       "zh-CN",
	}); err != nil {
		return fmt.Errorf("item/watched DELETE: %w", err)
	}
	return nil
}
