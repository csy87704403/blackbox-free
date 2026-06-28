package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	imageTagPattern = regexp.MustCompile(`(?is)<image\s+[^>]*path=["']([^"']+)["'][^>]*>.*?</image>`)
	atImagePattern  = regexp.MustCompile(`(?i)@([^\s<>"']+\.(?:png|jpe?g|webp|gif))`)
	largeGapPattern = regexp.MustCompile(`\n{3,}`)
	imageMIMETypes  = map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".webp": "image/webp",
		".gif":  "image/gif",
	}
)

func expandReasonixImages(body map[string]any, cfg config) error {
	if cfg.reasonixWorkspace == "" {
		return nil
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		return nil
	}

	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		content, ok := message["content"].(string)
		if !ok {
			continue
		}

		parts, err := imageParts(content, cfg)
		if err != nil {
			return err
		}
		if len(parts) == 0 {
			continue
		}

		text := imageTagPattern.ReplaceAllString(content, " ")
		text = atImagePattern.ReplaceAllString(text, " ")
		text = strings.TrimSpace(largeGapPattern.ReplaceAllString(text, "\n\n"))
		if text == "" {
			text = "请分析附加图片。"
		}
		contentParts := []any{map[string]any{"type": "text", "text": text}}
		contentParts = append(contentParts, parts...)
		message["content"] = contentParts
		messages[index] = message
	}
	body["messages"] = messages
	return nil
}

func imageParts(content string, cfg config) ([]any, error) {
	references := make([]string, 0)
	for _, match := range imageTagPattern.FindAllStringSubmatch(content, -1) {
		references = append(references, match[1])
	}
	for _, match := range atImagePattern.FindAllStringSubmatch(content, -1) {
		references = append(references, match[1])
	}

	seen := make(map[string]bool)
	parts := make([]any, 0)
	for _, reference := range references {
		filePath, mimeType, ok := resolveAttachment(reference, cfg)
		if !ok || seen[filePath] {
			continue
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}
		seen[filePath] = true
		parts = append(parts, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	return parts, nil
}

func resolveAttachment(reference string, cfg config) (string, string, bool) {
	workspace, err := filepath.Abs(cfg.reasonixWorkspace)
	if err != nil {
		return "", "", false
	}
	root := filepath.Join(workspace, ".reasonix", "attachments")
	cleaned := strings.TrimPrefix(strings.TrimSpace(reference), "@")
	cleaned = filepath.FromSlash(strings.ReplaceAll(cleaned, "\\", "/"))
	candidate := cleaned
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workspace, candidate)
	}
	candidate, err = filepath.Abs(filepath.Clean(candidate))
	if err != nil || !withinRoot(root, candidate) {
		return "", "", false
	}
	mimeType, ok := imageMIMETypes[strings.ToLower(filepath.Ext(candidate))]
	if !ok {
		return "", "", false
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > cfg.maxImageBytes {
		return "", "", false
	}
	return candidate, mimeType, true
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}

func imageCapability(cfg config) string {
	if cfg.reasonixWorkspace == "" {
		return "image_url_only"
	}
	return fmt.Sprintf("image_url_and_local_reasonix_attachments")
}
