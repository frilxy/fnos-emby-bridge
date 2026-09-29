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
	client := fn.NewClient(os.Getenv("FNOS_BASE"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Login(ctx, os.Getenv("FNOS_USER"), fn.SHA256Hex(os.Getenv("FNOS_PASS"))); err != nil {
		panic(err)
	}
	for _, guid := range os.Args[1:] {
		list, err := client.StreamListByGuid(ctx, guid)
		if err != nil {
			fmt.Println(guid, "error:", err)
			continue
		}
		b, _ := json.MarshalIndent(list, "", "  ")
		fmt.Println(guid, string(b))
	}
}
