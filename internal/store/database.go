package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"gopaste/internal/types"
)

// openDatabase 创建数据库并将旧版历史与设置一次性原子迁移
func (s *Store) openDatabase() (version int, err error) {
	s.db = connectDatabase(filepath.Join(s.dir, databaseFile), s.cipher.database)
	defer func() {
		if err != nil {
			s.db.Close()
		}
	}()
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	if version > schemaVersion {
		return 0, errors.New("store: unsupported database version")
	}
	if _, err = s.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return 0, err
	}
	var statements []statement
	if version == 0 {
		statements = []statement{
			{SQL: `CREATE TABLE records (id TEXT PRIMARY KEY, position INTEGER NOT NULL, kind TEXT NOT NULL, list_id TEXT NOT NULL, meta TEXT NOT NULL, summary TEXT NOT NULL, payload TEXT NOT NULL, search TEXT NOT NULL, extensions TEXT NOT NULL)`},
			{SQL: "CREATE INDEX records_order ON records(position DESC)"},
			{SQL: "CREATE INDEX records_list ON records(list_id, position DESC)"},
		}
		data, readErr := os.ReadFile(filepath.Join(s.dir, indexFile))
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return 0, readErr
		}
		if readErr == nil {
			var idx index
			if err = json.Unmarshal(data, &idx); err != nil {
				return 0, err
			}
			if idx.Version != 1 {
				return 0, errors.New("store: unsupported history index")
			}
			for i, r := range idx.Records {
				if r == nil || r.ID == "" {
					continue
				}
				if r.Hash == "" {
					r.Hash = recordHash(r)
				}
				row, e := encodeRecord(r)
				if e != nil {
					return 0, e
				}
				statements = append(statements, row.statement(int64(len(idx.Records)-i)))
			}
		}
	}
	if version < 2 {
		data, readErr := os.ReadFile(filepath.Join(s.dir, legacySettingsFile))
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return 0, readErr
		}
		if readErr == nil {
			s.settings.ExcludedAppsInitialized = false
			if err = json.Unmarshal(data, &s.settings); err != nil {
				return 0, err
			}
		}
		normalizeSettings(&s.settings)
		settingsRow, e := settingsStatement(s.settings)
		if e != nil {
			return 0, e
		}
		statements = append(statements,
			statement{SQL: "CREATE TABLE settings (id INTEGER PRIMARY KEY CHECK(id=1), data TEXT NOT NULL)"},
			settingsRow,
			statement{SQL: "PRAGMA user_version=2"},
		)
		if err = transaction(s.db, statements); err != nil {
			return 0, err
		}
	}
	var data []byte
	if err := s.db.QueryRow("SELECT data FROM settings WHERE id=1").Scan(&data); err != nil {
		return 0, err
	}
	if err := json.Unmarshal(data, &s.settings); err != nil {
		return 0, err
	}
	normalizeSettings(&s.settings)
	// 重置中断的链接预览以便下次调度
	if _, err = s.db.Exec(`UPDATE records SET
		meta=json_remove(meta,'$.previewState'),
		summary=json_remove(summary,'$.previewState'),
		payload=json_remove(payload,'$.previewState')
		WHERE json_extract(meta,'$.previewState')='loading'`); err != nil {
		return 0, err
	}
	rows, err := s.db.Query(`SELECT meta, position,
		octet_length(id) + octet_length(kind) + octet_length(list_id) + ? +
		octet_length(meta) + octet_length(summary) + octet_length(payload) + octet_length(search) + octet_length(extensions)
		FROM records ORDER BY position DESC`, positionBytes)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	s.positions = make(map[string]int64)
	for rows.Next() {
		var position int64
		r := new(types.Record)
		if err := rows.Scan(&data, &position, &r.DatabaseBytes); err != nil {
			return 0, err
		}
		if err = json.Unmarshal(data, r); err != nil {
			return 0, err
		}
		s.records = append(s.records, r)
		s.byID[r.ID] = r
		s.positions[r.ID] = position
	}
	return version, rows.Err()
}

// settingsStatement 生成整份应用设置的原子保存语句
func settingsStatement(settings types.Settings) (statement, error) {
	data, err := json.Marshal(settings)
	if err != nil {
		return statement{}, err
	}
	return statement{
		SQL:  "INSERT INTO settings(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data",
		Args: []any{string(data)},
	}, nil
}

// recordMeta 提取去重、排序和资源清理所需的常驻元数据
func recordMeta(r *types.Record) *types.Record {
	return &types.Record{ID: r.ID, Kind: r.Kind, CreatedAt: r.CreatedAt, ListID: r.ListID, Hash: r.Hash, Image: r.Image, PreviewImage: r.PreviewImage, PreviewState: r.PreviewState, Source: types.Source{Icon: r.Source.Icon}}
}

// positionBytes 表示记录排序字段的逻辑字节数
const positionBytes = 8

// recordRow 保存待写入的数据库字段与用于容量清理的元数据
type recordRow struct {
	record                                     *types.Record
	meta, summary, payload, search, extensions []byte
}

// encodeRecord 编码历史记录的数据库字段并按实际编码字节计算记录容量
func encodeRecord(r *types.Record) (recordRow, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return recordRow{}, err
	}
	record := recordMeta(r)
	meta, err := json.Marshal(record)
	if err != nil {
		return recordRow{}, err
	}
	summary := *r
	summary.Text = summaryText(r.Text)
	summary.HTML = ""
	summary.Title = summaryText(r.Title)
	summary.URL = summaryText(r.URL)
	summary.FileCount = len(r.Files)
	summary.Files = r.Files[:min(3, len(r.Files))]
	preview, err := json.Marshal(summary)
	if err != nil {
		return recordRow{}, err
	}
	fields := []string{r.Text, r.Title, r.URL, r.Color, r.Source.AppName}
	extensions := []string{}
	if r.Kind == types.KindImage {
		extensions = append(extensions, strings.ToLower(r.ImageFormat))
		if r.ImageFormat == "jpeg" {
			extensions = append(extensions, "jpg")
		}
	}
	for _, f := range r.Files {
		fields = append(fields, f.Name, f.Path)
		if !f.Dir {
			// 保存所有点号后缀以保留 tar.gz 等复合扩展名规则
			name := strings.ToLower(f.Name)
			for i, c := range name {
				if c == '.' && i+1 < len(name) {
					extensions = append(extensions, name[i+1:])
				}
			}
		}
	}
	for i := range fields {
		fields[i] = strings.ToLower(fields[i])
	}
	search, _ := json.Marshal(fields)
	exts, _ := json.Marshal(extensions)
	record.DatabaseBytes = int64(len(r.ID) + len(r.Kind) + len(r.ListID) + positionBytes + len(meta) + len(preview) + len(payload) + len(search) + len(exts))
	return recordRow{record: record, meta: meta, summary: preview, payload: payload, search: search, extensions: exts}, nil
}

// statement 按指定顺序写入已编码和计量的历史记录
func (r recordRow) statement(position int64) statement {
	return statement{SQL: `INSERT INTO records(id,position,kind,list_id,meta,summary,payload,search,extensions) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET position=excluded.position,kind=excluded.kind,list_id=excluded.list_id,meta=excluded.meta,summary=excluded.summary,payload=excluded.payload,search=excluded.search,extensions=excluded.extensions`, Args: []any{r.record.ID, position, string(r.record.Kind), r.record.ListID, string(r.meta), string(r.summary), string(r.payload), string(r.search), string(r.extensions)}}
}

// summaryText 截取卡片所需的前五百一十二个字符并保留截断提示
func summaryText(text string) string {
	text = strings.TrimSpace(text)
	n := 0
	for i := range text {
		if n == 512 {
			return strings.Clone(text[:i]) + "…"
		}
		n++
	}
	return text
}

// readRecordsLocked 从指定内容列读取记录
func (s *Store) readRecordsLocked(column, clause string, args ...any) ([]*types.Record, error) {
	rows, err := s.db.Query("SELECT "+column+" FROM records "+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []*types.Record
	for rows.Next() {
		r := new(types.Record)
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, r); err != nil {
			return nil, err
		}
		r.Summary = column == "summary"
		records = append(records, r)
	}
	return records, rows.Err()
}

// commitLocked 原子保存附加语句、记录与顺序，成功后更新元数据并回收资源
func (s *Store) commitLocked(next []*types.Record, row *recordRow, statements ...statement) error {
	candidates := references(s.records)
	if row != nil {
		for name := range references([]*types.Record{row.record}) {
			candidates[name] = 0
		}
	}
	positions := make(map[string]int64, len(next))
	var top int64
	for _, p := range s.positions {
		top = max(top, p)
	}
	kept := make(map[string]*types.Record, len(next))
	for i, r := range next {
		position := s.positions[r.ID]
		if i == 0 && (len(s.records) == 0 || s.records[0].ID != r.ID) {
			position = top + 1
		}
		if row != nil && r == row.record {
			statements = append(statements, row.statement(position))
		} else if position != s.positions[r.ID] {
			statements = append(statements, statement{SQL: "UPDATE records SET position=? WHERE id=?", Args: []any{position, r.ID}})
		}
		positions[r.ID] = position
		kept[r.ID] = r
	}
	for id := range s.byID {
		if kept[id] == nil {
			statements = append(statements, statement{SQL: "DELETE FROM records WHERE id=?", Args: []any{id}})
		}
	}
	if len(statements) > 0 {
		if err := transaction(s.db, statements); err != nil {
			return err
		}
	}
	s.records, s.byID, s.positions = next, kept, positions
	s.removeUnusedLocked(candidates)
	return nil
}

// IDs 返回符合搜索和列表规则的有序记录标识
func (s *Store) IDs(tab types.Tab, query string) ([]*types.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	where, args := queryClause(tab, query)
	rows, err := s.db.Query("SELECT id FROM records WHERE "+where+" ORDER BY position DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []*types.Record
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		records = append(records, &types.Record{ID: id})
	}
	return records, rows.Err()
}

// Summaries 按指定标识批量读取当前页的卡片摘要
func (s *Store) Summaries(ids []string) ([]*types.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return s.readRecordsLocked("summary", "WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", args...)
}

// PendingLinks 返回尚未获取预览的完整链接记录
func (s *Store) PendingLinks() ([]*types.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readRecordsLocked("payload", `WHERE kind='link' AND coalesce(json_extract(meta,'$.previewState'),'')=''`)
}

// queryClause 使用参数化 SQL 保留 Unicode 子串和智能列表匹配规则
func queryClause(tab types.Tab, query string) (string, []any) {
	conditions := []string{"1"}
	var args []any
	keyword := func(value string) string {
		args = append(args, strings.ToLower(value))
		return "EXISTS (SELECT 1 FROM json_each(search) WHERE instr(value,?)>0)"
	}
	if tab.ID != "all" {
		if !tab.Smart {
			conditions = append(conditions, "list_id=?")
			args = append(args, tab.ID)
		} else {
			rules := []string{}
			for _, rule := range tab.Rules {
				switch rule.Field {
				case types.RuleKind:
					rules = append(rules, "kind=?")
					args = append(args, rule.Value)
				case types.RuleKeyword:
					rules = append(rules, keyword(rule.Value))
				case types.RuleExtension:
					exts := []string{}
					for _, ext := range strings.FieldsFunc(strings.ToLower(rule.Value), func(r rune) bool { return r == ',' || r == '，' || unicode.IsSpace(r) }) {
						ext = strings.TrimPrefix(ext, ".")
						if ext == "" {
							continue
						}
						exts = append(exts, "value=?")
						args = append(args, ext)
					}
					if len(exts) == 0 {
						rules = append(rules, "0")
					} else {
						rules = append(rules, "EXISTS (SELECT 1 FROM json_each(extensions) WHERE "+strings.Join(exts, " OR ")+")")
					}
				default:
					rules = append(rules, "0")
				}
			}
			join := " AND "
			if tab.MatchAny {
				join = " OR "
			}
			if len(rules) == 0 {
				conditions = append(conditions, "0")
			} else {
				conditions = append(conditions, "("+strings.Join(rules, join)+")")
			}
		}
	}
	if q := strings.TrimSpace(query); q != "" {
		conditions = append(conditions, keyword(q))
	}
	return strings.Join(conditions, " AND "), args
}
