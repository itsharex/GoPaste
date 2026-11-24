package app

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"sync"
	"time"

	"gopaste/internal/linkprev"
	"gopaste/internal/store"
	"gopaste/internal/types"
)

// previewQueue 合并同一记录的待办链接，按需启动最多两个下载线程
type previewQueue struct {
	mu      sync.Mutex
	pending map[string]string
	active  int
	wg      sync.WaitGroup
}

// scheduleLinkPreviews 调度尚未获取标题与图片的链接预览
func (a *App) scheduleLinkPreviews() {
	if a.st == nil || !a.st.Settings().LinkPreview {
		return
	}
	records, err := a.st.PendingLinks()
	if err != nil {
		log.Print("reading pending previews: ", err)
		return
	}
	for _, r := range records {
		if r.Kind == types.KindLink && r.PreviewState == "" && r.URL != "" {
			a.queueLinkPreview(r.ID, r.URL)
		}
	}
}

// queueLinkPreview 将链接加入待办集合，已有两个下载线程时由它们继续消费
func (a *App) queueLinkPreview(id, url string) {
	q := &a.previews
	q.mu.Lock()
	defer q.mu.Unlock()
	if a.ctx.Err() != nil {
		return
	}
	q.pending[id] = url
	if q.active < 2 {
		q.active++
		q.wg.Go(a.runLinkPreviews)
	}
}

// runLinkPreviews 在锁内领取任务，队列清空或应用退出时释放线程名额
func (a *App) runLinkPreviews() {
	q := &a.previews
	for {
		q.mu.Lock()
		if len(q.pending) == 0 || a.ctx.Err() != nil {
			q.active--
			q.mu.Unlock()
			return
		}
		var id, url string
		for id, url = range q.pending {
			delete(q.pending, id)
			break
		}
		q.mu.Unlock()
		a.fetchPreview(id, url)
	}
}

// fetchPreview 获取链接标题与图片，退出中断的任务保留加载状态供重启恢复
func (a *App) fetchPreview(id, url string) {
	if a.ctx.Err() != nil || !a.st.Settings().LinkPreview {
		return
	}
	started := false
	_, ok := a.st.Update(id, func(r *types.Record) {
		if r.PreviewState == "" {
			r.PreviewState = "loading"
			started = true
		}
	})
	if !ok || !started {
		return
	}
	a.refresh()

	ctx, cancel := context.WithTimeout(a.ctx, 12*time.Second)
	defer cancel()
	res, err := linkprev.Fetch(ctx, url)
	if err != nil {
		if a.ctx.Err() != nil {
			return
		}
		a.st.Update(id, func(r *types.Record) { r.PreviewState = "failed" })
		a.refresh()
		return
	}
	var image []byte
	var ext string
	if res.ImageURL != "" {
		image, ext, _ = linkprev.DownloadImage(ctx, res.ImageURL)
	}
	if a.ctx.Err() != nil {
		return
	}
	apply := func(r *types.Record) {
		if res.Title != "" {
			r.Title = res.Title
		}
		if res.SiteName != "" && r.Source.AppName == "" {
			r.Source.AppName = res.SiteName
		}
		r.PreviewState = "done"
	}
	_, _, err = a.st.UpdatePreview(id, image, ext, apply)
	if len(image) > 0 && errors.Is(err, store.ErrHistoryLimit) {
		_, _, err = a.st.UpdatePreview(id, nil, "", apply)
	}
	if errors.Is(err, store.ErrHistoryLimit) {
		_, _, err = a.st.UpdatePreview(id, nil, "", func(r *types.Record) { r.PreviewState = "failed" })
	}
	if err != nil && !errors.Is(err, fs.ErrClosed) {
		log.Print("saving link preview: ", err)
	}
	a.refresh()
}
