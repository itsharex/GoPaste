package clip

/*
#cgo LDFLAGS: -luser32
#include <windows.h>
#include <stdint.h>
#include <string.h>

// writeBitmap 在同一次剪贴板写入中发布 Windows 原生位图、原始 PNG 和来源标记
static DWORD writeBitmap(uintptr_t hwnd, const void *dib, size_t dibSize, const void *png, size_t pngSize, const WCHAR *origin) {
	if (!hwnd) return ERROR_INVALID_WINDOW_HANDLE;
	uint64_t length = pngSize;
	struct { UINT format; const void *bytes; SIZE_T size; HGLOBAL memory; } parts[] = {
		{CF_DIBV5, dib, dibSize, NULL},
		{RegisterClipboardFormatW(L"PNG"), png, pngSize, NULL},
		{RegisterClipboardFormatW(L"image/png"), png, pngSize, NULL},
		{RegisterClipboardFormatW(L"MyGo.Transfer.Length:image/png"), &length, sizeof(length), NULL},
		{RegisterClipboardFormatW(origin), "GoPaste", sizeof("GoPaste") - 1, NULL},
	};
	DWORD error = ERROR_SUCCESS;
	for (size_t i = 0; i < ARRAYSIZE(parts); i++) {
		if (!parts[i].format) { error = ERROR_INVALID_PARAMETER; goto done; }
		parts[i].memory = GlobalAlloc(GHND, parts[i].size);
		if (!parts[i].memory) { error = ERROR_NOT_ENOUGH_MEMORY; goto done; }
		void *p = GlobalLock(parts[i].memory);
		if (!p) { error = GetLastError(); goto done; }
		memcpy(p, parts[i].bytes, parts[i].size);
		GlobalUnlock(parts[i].memory);
	}

	ULONGLONG deadline = GetTickCount64() + 1000;
	while (!OpenClipboard((HWND)hwnd)) {
		if (GetTickCount64() >= deadline) { error = ERROR_BUSY; goto done; }
		MSG msg;
		MsgWaitForMultipleObjectsEx(0, NULL, 10, QS_SENDMESSAGE, 0);
		PeekMessageW(&msg, NULL, 0, 0, PM_NOREMOVE | PM_QS_SENDMESSAGE);
	}
	if (!EmptyClipboard()) {
		error = GetLastError();
	} else {
		for (size_t i = 0; i < ARRAYSIZE(parts); i++) {
			if (!SetClipboardData(parts[i].format, parts[i].memory)) {
				error = GetLastError();
				EmptyClipboard();
				break;
			}
			parts[i].memory = NULL;
		}
	}
	if (!CloseClipboard() && !error) error = GetLastError();
done:
	for (size_t i = 0; i < ARRAYSIZE(parts); i++) {
		if (parts[i].memory) GlobalFree(parts[i].memory);
	}
	return error;
}
*/
import "C"

import (
	"os"
	"slices"
	"syscall"
	"unsafe"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
)

// Write 通过系统剪贴板发布单张 PNG 的原生位图格式并保留其他内容的传输方式
func Write(win *mygo.Window, data transfer.Data) error {
	if len(data.Items()) != 1 || !slices.Equal(data.Formats(), []transfer.Format{transfer.PNG}) {
		return mygo.Clipboard.Write(markOwnClipboard(data))
	}
	raw, err := data.Read(transfer.PNG)
	if err != nil {
		return err
	}
	dib, err := pngDIBV5(raw)
	if err != nil {
		return err
	}
	origin, err := syscall.UTF16PtrFromString(string(ownClipboardFormat))
	if err != nil {
		return err
	}
	mygo.RunOnMain(func() {
		code := C.writeBitmap(C.uintptr_t(win.NativeHandle()), unsafe.Pointer(&dib[0]), C.size_t(len(dib)),
			unsafe.Pointer(&raw[0]), C.size_t(len(raw)), (*C.WCHAR)(unsafe.Pointer(origin)))
		if code != 0 {
			err = os.NewSyscallError("write clipboard bitmap", syscall.Errno(code))
		}
	})
	return err
}
