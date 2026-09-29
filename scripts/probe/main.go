// Command probe 是本地只读接口探测工具，仅用于定位飞牛响应结构。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"fnos-emby-bridge/internal/fn"
)

func main() {
	base := os.Getenv("FNOS_BASE")
	user := os.Getenv("FNOS_USER")
	pass := os.Getenv("FNOS_PASS")
	client := fn.NewClient(base)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Login(ctx, user, fn.SHA256Hex(pass)); err != nil {
		panic(err)
	}
	dbs, err := client.MediaDBList(ctx)
	if err != nil {
		panic(err)
	}
	for _, db := range dbs {
		fmt.Printf("\nLIB %s %s %+v\n", db.Guid, db.Title, db)
		items, _, err := client.ItemListPage(ctx, "ancestor_guid", db.Guid, 1, 100)
		if err != nil {
			fmt.Println("item/list:", err)
			continue
		}
	for _, item := range items {
			if item.Type != "Episode" && item.Type != "Movie" && item.Type != "Video" {
				fmt.Printf("ITEM %s %s %s\n", item.Guid, item.Type, item.Title)
				continue
			}
			fmt.Printf("PLAYABLE %s %s %s\n", item.Guid, item.Type, item.Title)
			if raw, err := client.ProbeJSON(ctx, "play/info", map[string]any{"item_guid": item.Guid}); err != nil {
				fmt.Println("  raw play err:", err)
			} else {
				dump("raw-play-info", json.RawMessage(raw))
			}
			if parent := item.ParentGuid; parent != "" {
				fmt.Println("PARENT", parent)
				siblings, err := client.ItemList(ctx, parent)
				if err != nil {
					fmt.Println("  parent list err:", err)
				} else {
					fmt.Printf("  siblings %d\n", len(siblings))
					for _, s := range siblings {
						fmt.Printf("  sibling %s type=%s title=%s parent=%s\n", s.Guid, s.Type, s.Title, s.ParentGuid)
					}
				}
			}
			if info, err := client.PlayInfoByGuid(ctx, item.Guid); err != nil {
				fmt.Println("  play info err:", err)
			} else {
				_ = info
			}
		}
	}
}

func dump(label string, value any) {
	b, _ := json.MarshalIndent(value, "  ", "  ")
	fmt.Printf("  %s: %s\n", label, b)
}
