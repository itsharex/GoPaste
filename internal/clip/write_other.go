//go:build !windows

package clip

import (
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
)

// Write 为本次复制添加来源标记并写入系统剪贴板
func Write(_ *mygo.Window, data transfer.Data) error {
	return mygo.Clipboard.Write(markOwnClipboard(data))
}
