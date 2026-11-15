// Command actorart 把作品目录里的本地演员图「灌」进飞牛人物库。
//
// 背景：飞牛「其他」类型库不刮演员头像，person.profile 恒为空；而这类库的演员图
// 通常由刮削工具（MDC / JavSP）按 Kodi/Emby 约定写在影片目录的
// `.actors/<演员名>.jpg`。飞牛人物编辑器本身支持上传头像，所以脚本可以：
//
//	读 .actors 图片 → POST image/temp/upload 拿 hash_path → 写回 person.profile_path
//
// 跑完之后飞牛影视本体会显示头像，桥接也能直接从 person.profile 拿到，不需要任何
// 权限调整或改桥接代码。
//
// 需要以 root 运行：.actors 里的图片通常是刮削工具（另一个应用用户）创建的 700 文件，
// 普通用户读不到。
//
// 用法：
//
//	sudo ./actorart -config /vol5/@appdata/fnos-emby-bridge/bridge-config.json        # 只列清单
//	sudo ./actorart -config ... -apply                                               # 真正写入（先备份）
//	sudo ./actorart -restore actorart-backup-20261116-004500.json                    # 回滚
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fnos-emby-bridge/internal/admin"
	"fnos-emby-bridge/internal/fn"
)

// 演员图目录名与扩展名：与 Kodi / Emby / MDC 的约定保持一致。
var (
	actorArtDirs = []string{".actors", ".actor", "actors"}
	actorArtExts = []string{".jpg", ".jpeg", ".png", ".webp", ".tbn"}
)

type candidate struct {
	PersonGuid string
	Name       string
	Original   string
	ItemGuid   string
	ItemTitle  string
	ImagePath  string
	ImageSize  int64
	ProfileNow string
}

type backupEntry struct {
	Name       string `json:"name"`
	ItemTitle  string `json:"item_title"`
	Image      string `json:"image"`
	OldProfile string `json:"old_profile"`
	NewProfile string `json:"new_profile"`
	At         string `json:"at"`
}

func main() {
	var (
		cfgPath  = flag.String("config", defaultConfigPath(), "桥接配置文件（bridge-config.json）路径")
		base     = flag.String("base", "", "飞牛地址，覆盖配置文件（如 http://127.0.0.1:5666）")
		user     = flag.String("user", "", "飞牛账号，覆盖配置文件")
		pass     = flag.String("pass", "", "飞牛密码，覆盖配置文件")
		apply    = flag.Bool("apply", false, "真正写入（默认只打印清单）")
		limit    = flag.Int("limit", 0, "最多处理多少个演员（0 = 不限）")
		only     = flag.String("only", "", "只处理指定 person guid（逗号分隔）")
		backupTo = flag.String("backup", "", "备份文件路径（默认 actorart-backup-<时间>.json）")
		restore  = flag.String("restore", "", "回滚：从备份文件恢复 profile_path")
		verbose  = flag.Bool("v", false, "打印详细过程")
	)
	flag.Parse()

	ctx := context.Background()
	if *restore != "" {
		if err := runRestore(ctx, *restore, *cfgPath, *base, *user, *pass, *verbose); err != nil {
			fmt.Fprintln(os.Stderr, "回滚失败:", err)
			os.Exit(1)
		}
		return
	}

	if *cfgPath == "bridge.config.json" {
		// 没找到：把应用数据目录的内容打出来，方便定位真实路径
		for _, vol := range []string{"/vol1", "/vol2", "/vol3", "/vol4", "/vol5", "/vol6"} {
			root := vol + "/@appdata/fnos-emby-bridge"
			if entries, err := os.ReadDir(root); err == nil {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				fmt.Fprintf(os.Stderr, "[诊断] %s 下有: %s\n", root, strings.Join(names, ", "))
			}
		}
	}
	fmt.Fprintf(os.Stderr, "使用配置：%s\n", *cfgPath)
	client, err := login(ctx, *cfgPath, *base, *user, *pass)
	if err != nil {
		fmt.Fprintln(os.Stderr, "登录飞牛失败:", err)
		os.Exit(1)
	}

	cands, err := collect(ctx, client, *only, *limit, *verbose)
	if err != nil {
		fmt.Fprintln(os.Stderr, "收集失败:", err)
		os.Exit(1)
	}
	if len(cands) == 0 {
		fmt.Println("没有找到需要补头像的演员（要么都有头像了，要么目录里没有 .actors 图片）")
		return
	}

	fmt.Printf("找到 %d 个演员可补头像：\n\n", len(cands))
	for i, c := range cands {
		fmt.Printf("%3d) %-16s %-40s %8d B  %s\n", i+1, c.Name, c.ItemTitle, c.ImageSize, c.ImagePath)
	}
	if !*apply {
		fmt.Printf("\n这是 dry-run。确认无误后加 -apply 执行（会先写备份）。\n")
		return
	}

	if *backupTo == "" {
		*backupTo = fmt.Sprintf("actorart-backup-%s.json", time.Now().Format("20060102-150405"))
	}
	backup := map[string]backupEntry{}
	ok, failed := 0, 0
	for i, c := range cands {
		if err := applyOne(ctx, client, c, backup); err != nil {
			failed++
			fmt.Printf("[%d/%d] ✗ %s: %v\n", i+1, len(cands), c.Name, err)
			continue
		}
		ok++
		fmt.Printf("[%d/%d] ✓ %s → %s\n", i+1, len(cands), c.Name, c.ImagePath)
		if err := saveBackup(*backupTo, backup); err != nil {
			fmt.Fprintln(os.Stderr, "写备份失败:", err)
		}
	}
	fmt.Printf("\n完成：成功 %d，失败 %d。备份：%s\n", ok, failed, *backupTo)
}

// readEnvFile 读 KEY=VALUE 形式的 env 文件（bridge.env），忽略注释与空行，
// 去掉值两侧的单/双引号。文件不存在时返回空 map。
func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(strings.Trim(v, `"'`))
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// defaultConfigPath 猜测桥接配置位置（fnOS 应用数据目录）。
// fnOS 的 TRIM_PKGETC 可能是 @appdata/<app> 下的任意子目录，所以找不到就直接
// 在应用数据目录里浅递归找 bridge.env / bridge-config.json（工具本身以 root 跑）。
func defaultConfigPath() string {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		return p
	}
	if p := configFromProcess(); p != "" {
		return p
	}
	for _, vol := range []string{"/vol1", "/vol2", "/vol3", "/vol4", "/vol5", "/vol6"} {
		root := vol + "/@appdata/fnos-emby-bridge"
		if p := findConfigFile(root, 0); p != "" {
			return p
		}
	}
	return "bridge.config.json"
}

var configNames = []string{"bridge.env", "bridge-config.json"}

// configFromProcess 从运行中的桥接进程环境变量里取配置目录：
//
//	CONFIG_PATH 直接是配置文件；否则 TRIM_PKGETC 下找 bridge.env / bridge-config.json。
//
// 需要 root（读别的用户的 /proc/<pid>/environ）。
func configFromProcess() string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		pid := e.Name()
		if pid == "" || pid[0] < '0' || pid[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil || !strings.Contains(string(cmdline), "fnos-emby-bridge/server") {
			continue
		}
		raw, err := os.ReadFile("/proc/" + pid + "/environ")
		if err != nil {
			continue
		}
		env := map[string]string{}
		for _, kv := range strings.Split(string(raw), "\x00") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
		if p := env["CONFIG_PATH"]; p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		if etc := env["TRIM_PKGETC"]; etc != "" {
			for _, n := range configNames {
				p := filepath.Join(etc, n)
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
	}
	return ""
}

// findConfigFile 在 root 下最多 3 层内找配置文件；优先 bridge.env，其次 bridge-config.json。
// 同一层有多个时取修改时间最新的。
func findConfigFile(root string, depth int) string {
	if depth > 3 {
		return ""
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return ""
	}
	for _, name := range configNames {
		p := filepath.Join(root, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if p := findConfigFile(filepath.Join(root, e.Name()), depth+1); p != "" {
			return p
		}
	}
	return ""
}

func login(ctx context.Context, cfgPath, base, user, pass string) (*fn.Client, error) {
	if base == "" || user == "" || pass == "" {
		// 桥接的凭据有两处可能：bridge.env（向导写入，键 FNOS_*）
		// 或 bridge-config.json（后台页面改过的连接）。两个都试。
		env := readEnvFile(cfgPath)
		if base == "" {
			base = env["FNOS_BASE"]
		}
		if user == "" {
			user = env["FNOS_USER"]
		}
		if pass == "" {
			pass = env["FNOS_PASS"]
		}
	}
	if base == "" || user == "" || pass == "" {
		if cc := admin.LoadStore(cfgPath).Connection(); cc != nil {
			if base == "" {
				base = cc.Base
			}
			if user == "" {
				user = cc.User
			}
			if pass == "" {
				pass = cc.Pass
			}
		}
	}
	if base == "" || user == "" || pass == "" {
		return nil, fmt.Errorf("在 %s 里没找到连接信息：bridge.env 需含 FNOS_BASE/FNOS_USER/FNOS_PASS，或 bridge-config.json 需含 connection；也可用 -base/-user/-pass 直接指定", cfgPath)
	}
	c := fn.NewClient(base)
	if err := c.Login(ctx, user, fn.SHA256Hex(pass)); err != nil {
		return nil, err
	}
	return c, nil
}

// collect 遍历媒体库 → 作品 → 人物，找出「没有头像、但目录里有本地图」的演员。
func collect(ctx context.Context, client *fn.Client, only string, limit int, verbose bool) ([]candidate, error) {
	onlySet := map[string]bool{}
	for _, g := range strings.Split(only, ",") {
		if g = strings.TrimSpace(g); g != "" {
			onlySet[g] = true
		}
	}

	libs, err := client.MediaDBList(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []candidate
	for _, lib := range libs {
		items, err := client.ItemList(ctx, lib.Guid)
		if err != nil {
			if verbose {
				fmt.Fprintf(os.Stderr, "[warn] 库 %s 列表失败: %v\n", lib.Title, err)
			}
			continue
		}
		// 拿人物：电影/视频直接查；剧的演职员挂在「季」级，退回季 guid 查。
		type personRef struct {
			person fn.Person
			item   fn.MediaItem
		}
		for _, it := range items {
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
			persons, err := client.PersonList(ctx, it.Guid)
			if err != nil || len(persons) == 0 {
				if it.Type == "TV" {
					persons = peopleOfSeasons(ctx, client, it, verbose)
				}
			}
			for _, p := range persons {
				if p.PersonGuid == "" || seen[p.PersonGuid] {
					continue
				}
				if len(onlySet) > 0 && !onlySet[p.PersonGuid] {
					continue
				}
				seen[p.PersonGuid] = true
				cand, ok := pickCandidate(ctx, client, it, p, verbose)
				if !ok {
					continue
				}
				out = append(out, cand)
				if limit > 0 && len(out) >= limit {
					return out, nil
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// peopleOfSeasons 剧的演职员挂在季级：逐季查，返回第一个非空季的演员表。
func peopleOfSeasons(ctx context.Context, client *fn.Client, series fn.MediaItem, verbose bool) []fn.Person {
	seasons, err := client.SeasonList(ctx, series.Guid)
	if err != nil {
		return nil
	}
	for _, s := range seasons {
		ps, err := client.PersonList(ctx, s.Guid)
		if err == nil && len(ps) > 0 {
			return ps
		}
	}
	return nil
}

// pickCandidate 找一个演员的本地图，并确认他当前还没头像。
func pickCandidate(ctx context.Context, client *fn.Client, item fn.MediaItem, p fn.Person, verbose bool) (candidate, bool) {
	pd, err := client.PersonDetail(ctx, p.PersonGuid)
	if err == nil && pd != nil && pd.Profile != "" {
		return candidate{}, false // 已经有头像，跳过
	}
	dir := mediaDir(ctx, client, item.Guid, verbose)
	if dir == "" {
		return candidate{}, false
	}
	names := actorNames(p.Name, p.OriginalName)
	img := matchActorArt(dir, names)
	if img == "" {
		img = matchActorArt(filepath.Dir(dir), names)
	}
	if img == "" {
		return candidate{}, false
	}
	st, err := os.Stat(img)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "[warn] %s 不可读: %v（需要 root 运行）\n", img, err)
		}
		return candidate{}, false
	}
	return candidate{
		PersonGuid: p.PersonGuid,
		Name:       p.Name,
		Original:   p.OriginalName,
		ItemGuid:   item.Guid,
		ItemTitle:  item.Title,
		ImagePath:  img,
		ImageSize:  st.Size(),
	}, true
}

func mediaDir(ctx context.Context, client *fn.Client, guid string, verbose bool) string {
	sl, err := client.StreamListByGuid(ctx, guid)
	if err != nil || sl == nil || len(sl.Files) == 0 {
		return ""
	}
	for _, f := range sl.Files {
		if f.Path != "" {
			return filepath.Dir(f.Path)
		}
	}
	return ""
}

// applyOne 上传图片并写回 profile_path；原值记进 backup。
func applyOne(ctx context.Context, client *fn.Client, c candidate, backup map[string]backupEntry) error {
	data, err := os.ReadFile(c.ImagePath)
	if err != nil {
		return fmt.Errorf("读图失败（需要 root？）: %w", err)
	}
	hash, err := client.UploadImage(ctx, filepath.Base(c.ImagePath), data)
	if err != nil {
		return err
	}
	detail, err := client.PersonEditDetail(ctx, c.PersonGuid)
	if err != nil {
		return err
	}
	old, _ := detail["profile_path"].(string)
	detail["profile_path"] = hash
	if err := client.SavePersonEditDetail(ctx, detail); err != nil {
		return err
	}
	backup[c.PersonGuid] = backupEntry{
		Name: c.Name, ItemTitle: c.ItemTitle, Image: c.ImagePath,
		OldProfile: old, NewProfile: hash, At: time.Now().Format(time.RFC3339),
	}
	return nil
}

func saveBackup(path string, backup map[string]backupEntry) error {
	b, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func runRestore(ctx context.Context, backupPath, cfgPath, base, user, pass string, verbose bool) error {
	b, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	var backup map[string]backupEntry
	if err := json.Unmarshal(b, &backup); err != nil {
		return fmt.Errorf("备份文件解析失败: %w", err)
	}
	client, err := login(ctx, cfgPath, base, user, pass)
	if err != nil {
		return err
	}
	ok, failed := 0, 0
	for guid, e := range backup {
		detail, err := client.PersonEditDetail(ctx, guid)
		if err != nil {
			failed++
			fmt.Printf("✗ %s: %v\n", e.Name, err)
			continue
		}
		detail["profile_path"] = e.OldProfile
		if err := client.SavePersonEditDetail(ctx, detail); err != nil {
			failed++
			fmt.Printf("✗ %s: %v\n", e.Name, err)
			continue
		}
		ok++
		fmt.Printf("✓ 已恢复 %s（profile_path=%q）\n", e.Name, e.OldProfile)
	}
	fmt.Printf("回滚完成：成功 %d，失败 %d\n", ok, failed)
	return nil
}

// ---- 名字与图片匹配 ----

func normActorName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	return strings.NewReplacer(" ", "", "\u3000", "", "_", "", ".", "", "-", "", "·", "").Replace(s)
}

func actorNames(name, original string) []string {
	out := make([]string, 0, 2)
	seen := map[string]struct{}{}
	for _, s := range []string{name, original} {
		if n := normActorName(s); n != "" {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

func hasActorArtExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, want := range actorArtExts {
		if ext == want {
			return true
		}
	}
	return false
}

// matchActorArt 在 dir 下的演员图目录里找匹配图片；同时支持
// `.actors/<演员名>.jpg` 与 `.actors/<演员名>/任意图片` 两种布局。
func matchActorArt(dir string, names []string) string {
	if dir == "" || len(names) == 0 {
		return ""
	}
	for _, d := range actorArtDirs {
		base := filepath.Join(dir, d)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !hasActorArtExt(e.Name()) {
				continue
			}
			stem := normActorName(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
			for _, want := range names {
				if want != "" && stem == want {
					return filepath.Join(base, e.Name())
				}
			}
		}
		for _, e := range entries {
			if !e.IsDir() || normActorName(e.Name()) == "" {
				continue
			}
			matched := false
			for _, want := range names {
				if want != "" && normActorName(e.Name()) == want {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			sub := filepath.Join(base, e.Name())
			if ins, err := os.ReadDir(sub); err == nil {
				for _, f := range ins {
					if !f.IsDir() && hasActorArtExt(f.Name()) {
						return filepath.Join(sub, f.Name())
					}
				}
			}
		}
	}
	return ""
}
