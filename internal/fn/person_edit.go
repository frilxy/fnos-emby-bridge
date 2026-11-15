package fn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
)

// 人物编辑相关接口（影视 web 前端逆向 1.12.x）：
//   POST /v/api/v1/person/getEditDetail   读可编辑字段
//   POST /v/api/v1/person/saveEditDetail  回写
//   POST /v/api/v1/image/temp/upload      传图，返回 hash_path（可直接当 profile_path）
//
// 飞牛「其他」类型库不刮演员头像，但人物编辑器本身支持上传头像——批量脚本就是
// 把作品目录里 .actors/<演员名>.jpg 传上去、再写回 profile_path。

// PersonEditDetail 读人物可编辑字段。字段随版本漂移，用 map 保留未知键：
// 回写时把原 map 带回去，避免把简介、锁标记等清空。
func (c *Client) PersonEditDetail(ctx context.Context, guid string) (map[string]any, error) {
	res, err := c.do(ctx, "POST", "person/getEditDetail", map[string]interface{}{"guid": guid})
	if err != nil {
		return nil, fmt.Errorf("person/getEditDetail %q: %w", guid, err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Data, &out); err != nil {
		return nil, fmt.Errorf("person/getEditDetail decode: %w", err)
	}
	if out == nil {
		return nil, fmt.Errorf("person/getEditDetail %q: 空响应", guid)
	}
	return out, nil
}

// SavePersonEditDetail 回写人物编辑详情。
func (c *Client) SavePersonEditDetail(ctx context.Context, body map[string]any) error {
	if _, err := c.do(ctx, "POST", "person/saveEditDetail", body); err != nil {
		return fmt.Errorf("person/saveEditDetail: %w", err)
	}
	return nil
}

// UploadImage 上传一张图片到飞牛临时图库，返回 hash_path（可用作 profile_path）。
//
// 契约来自前端上传控件：multipart 字段 file + image_type（人物头像用 poster）；
// 响应 {code,data:{hash_path}}。
// 签名按空 body 计算（与 GET 一致）——若某个版本对 multipart 也校验 body hash，
// 这里会在真机上返回 invalid sign，届时按实际签名规则调整。
func (c *Client) UploadImage(ctx context.Context, filename, imageType string, data []byte) (string, error) {
	if imageType == "" {
		imageType = "poster"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(filename))
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	// 前端 buildFormData 会带 image_type（poster/backdrop/logo/thumb）；
	// 缺了这个字段真机回 code=-1 Invalid Params。
	if err := mw.WriteField("image_type", imageType); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	fullPath := "/v/api/v1/image/temp/upload"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+fullPath, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authx", authx(fullPath, ""))
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", c.Token)
		req.Header.Set("Cookie", "Trim-MC-token="+c.Token+"; mode=relay")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			HashPath string `json:"hash_path"`
			Filename string `json:"filename"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("image/temp/upload decode (status=%d): %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if res.Code != 0 {
		return "", fmt.Errorf("image/temp/upload code=%d msg=%s", res.Code, res.Msg)
	}
	if res.Data.HashPath == "" {
		return "", fmt.Errorf("image/temp/upload 未返回 hash_path: %s", truncate(string(raw), 200))
	}
	return res.Data.HashPath, nil
}
