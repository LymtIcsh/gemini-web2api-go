package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// 原生网页的图片引用包含独立文件ID，生成请求必须保留该绑定字段。
func TestBrowserVideoImageAttachmentsCarryNativeFileIDs(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprintf("images_%d", count), func(t *testing.T) { testVideoImageBinding(t, count) })
	}
}

func testVideoImageBinding(t *testing.T, imageCount int) {
	id, _ := videoCapacityTestTransport(t, context.Background())
	ack, err := os.ReadFile("testdata/video_ack_raw.txt")
	if err != nil {
		t.Fatal(err)
	}
	generated, uploaded := 0, 0
	withBrowserProtocol(t, func(req browserRequest) []browserEvent {
		switch {
		case req.Op == "quota":
			return protocolAvailableQuota(t, req)
		case req.URL == "https://gemini.google.com/app":
			return protocolBody(`{"SNlM0e":"signed-in-token:12345","qKIAYe":"push-token","Ylro7b":"context-token"}`)
		case req.URL == uploadHost:
			events := protocolBody("")
			events[0].Headers = map[string]string{"x-goog-upload-url": "https://push.clients6.google.com/upload/test-finalize"}
			return events
		case req.URL == "https://push.clients6.google.com/upload/test-finalize":
			uploaded++
			if !bytes.Equal(videoBindingDecodeBody(t, req.BodyBase64), tinyPNG) {
				t.Error("图片上传的字节不一致")
			}
			return protocolBody("/contrib_service/ttl_1d/image-upload")
		case strings.Contains(req.URL, "/StreamGenerate?"):
			generated++
			form, err := url.ParseQuery(string(videoBindingDecodeBody(t, req.BodyBase64)))
			if err != nil {
				t.Fatal(err)
			}
			var outer []any
			if err := json.Unmarshal([]byte(form.Get("f.req")), &outer); err != nil {
				t.Fatal(err)
			}
			var inner []any
			if err := json.Unmarshal([]byte(outer[1].(string)), &inner); err != nil {
				t.Fatal(err)
			}
			input := inner[0].([]any)
			refs := input[3].([]any)
			if inner[49] != float64(toolVideo) {
				t.Errorf("图片请求没有路由到视频工具: %v", inner[49])
			}
			if len(refs) != imageCount {
				t.Fatalf("图片数量被改变: %d", len(refs))
			}
			if len(input) != 10 {
				t.Errorf("视频图片请求没有原生输入选项: 输入只有%d槽", len(input))
			} else {
				options, _ := json.Marshal(input[9])
				if string(options) != `[null,null,null,null,null,null,[[null,null,null,1]]]` {
					t.Errorf("视频输入选项偏离单图/双图原生抓包: %s", options)
				}
			}
			seen := map[string]bool{}
			for index, ref := range refs {
				metadata := ref.([]any)[0].([]any)
				if len(metadata) != 5 {
					t.Errorf("原生图片绑定需要5槽，当前只有%d槽", len(metadata))
					continue
				}
				if metadata[0] != "/contrib_service/ttl_1d/image-upload" || metadata[1] != float64(1) || metadata[2] != nil || metadata[3] != "image/png" {
					t.Errorf("引用路径、类型或MIME丢失: %v", metadata)
				}
				if ref.([]any)[1] != []string{"first.png", "last.png"}[index] {
					t.Errorf("图片顺序或文件名被改变: %v", ref.([]any)[1])
				}
				fileID, ok := metadata[4].(string)
				parsed, err := uuid.Parse(fileID)
				if !ok || err != nil || parsed.Version() != 4 || seen[fileID] {
					t.Errorf("每张图片必须有独立的有效文件ID: %q", fileID)
				}
				seen[fileID] = true
			}
			return protocolBody(string(ack))
		case strings.Contains(req.URL, "rpcids=hNvQHb"):
			return protocolBody(videoCapacityTestHistory("Your video is ready.", "", "https://contribution.usercontent.google.com/download?c=cmVzcG9uc2VfZGF0YQ"))
		case req.Op == "download":
			events := protocolBody(string([]byte{0, 0, 0, 32, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}))
			events[0].Headers = map[string]string{"content-type": "video/mp4"}
			return events
		default:
			t.Errorf("意外的外部请求: %s", req.URL)
			return protocolBody("")
		}
	})
	mc := Models["gemini-video"]
	mc.PinAccountID, mc.Context, mc.KeepConversation = id, context.Background(), true
	pending := []pendingUpload{{Data: tinyPNG, Name: "first.png", Kind: 1, Mime: "image/png"}, {Data: tinyPNG, Name: "last.png", Kind: 1, Mime: "image/png"}}
	_, _, result, err := callGemini("animate references", "animate references", mc, nil, pending[:imageCount], nil, nil)
	if err != nil || result == nil || len(result.Artifacts) != 1 || uploaded != imageCount || generated != 1 {
		t.Fatalf("图片视频调用链失败: uploads=%d generated=%d result=%+v error=%v", uploaded, generated, result, err)
	}
}

func videoBindingDecodeBody(t *testing.T, value string) []byte {
	t.Helper()
	body, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
