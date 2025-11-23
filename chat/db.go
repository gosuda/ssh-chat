package chat

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/iwanhae/ssh-chat/types"
	_ "github.com/mattn/go-sqlite3" // SQLite 드라이버
)

// MessageStore는 메시지 저장소에 대한 인터페이스입니다.
type MessageStore interface {
	Init() error
	AppendMessage(msg Message) error
	GetMessages(offset, limit int) ([]Message, error)
	GetMessageCount() (int, error)
	Close() error
	GetUserColor(nick string) (int, error)
	SetUserColor(nick string, color int) error
	CreateUser(nick string, color int) error
	GetBans() ([]*types.Ban, error)
	SaveBan(*types.Ban) error
	RemoveBan(*types.Ban) error
}

// SQLiteMessageStore는 SQLite를 사용하여 메시지를 저장합니다.
type SQLiteMessageStore struct {
	db *sql.DB
}

// NewSQLiteMessageStore는 새로운 SQLiteMessageStore 인스턴스를 생성합니다.
func NewSQLiteMessageStore(dataSourceName string) (*SQLiteMessageStore, error) {
	db, err := sql.Open("sqlite3", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("SQLite 데이터베이스 열기 실패: %w", err)
	}
	return &SQLiteMessageStore{db: db}, nil
}

// Init은 메시지 테이블을 초기화합니다.
func (s *SQLiteMessageStore) Init() error {
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		nick TEXT NOT NULL,
		text TEXT NOT NULL,
		color INTEGER NOT NULL,
		ip TEXT,
		mentions TEXT
	);`
	_, err := s.db.Exec(createTableSQL)
	if err != nil {
		return fmt.Errorf("메시지 테이블 생성 실패: %w", err)
	}

	createUserTableSQL := `
	CREATE TABLE IF NOT EXISTS users (
		nick TEXT PRIMARY KEY,
		color INTEGER NOT NULL
	);`
	_, err = s.db.Exec(createUserTableSQL)
	if err != nil {
		return fmt.Errorf("사용자 테이블 생성 실패: %w", err)
	}

	createBansTableSQL := `
	CREATE TABLE IF NOT EXISTS bans (
		ip TEXT PRIMARY KEY,
		banned_by TEXT NOT NULL,
		reason TEXT NOT NULL,
		timestamp DATETIME NOT NULL
	);`
	_, err = s.db.Exec(createBansTableSQL)
	if err != nil {
		return fmt.Errorf("밴 테이블 생성 실패: %w", err)
	}

	log.Println("SQLite 메시지, 사용자 및 밴 테이블 초기화 완료.")
	return nil
}

// AppendMessage는 메시지를 데이터베이스에 추가하고, 메시지 수가 4000개를 초과하면 가장 오래된 메시지를 삭제합니다.
func (s *SQLiteMessageStore) AppendMessage(msg Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("트랜잭션 시작 실패: %w", err)
	}

	mentions := ""
	if len(msg.Mentions) > 0 {
		for i, m := range msg.Mentions {
			mentions += m
			if i < len(msg.Mentions)-1 {
				mentions += ","
			}
		}
	}

	insertSQL := `INSERT INTO messages(timestamp, nick, text, color, ip, mentions) VALUES(?, ?, ?, ?, ?, ?)`
	_, err = tx.Exec(insertSQL, msg.Time, msg.Nick, msg.Text, msg.Color, msg.IP, mentions)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("메시지 데이터베이스 저장 실패: %w", err)
	}

	var count int
	err = tx.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("메시지 개수 조회 실패: %w", err)
	}

	if count > 4000 {
		deleteSQL := `DELETE FROM messages WHERE id IN (SELECT id FROM messages ORDER BY id ASC LIMIT ?)`
		_, err = tx.Exec(deleteSQL, count-4000)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("오래된 메시지 삭제 실패: %w", err)
		}
	}

	return tx.Commit()
}

// GetMessages는 지정된 offset과 limit에 따라 메시지를 조회합니다.
// offset은 최신 메시지부터의 오프셋입니다 (0은 가장 최신).
// limit은 가져올 메시지의 최대 개수입니다.
func (s *SQLiteMessageStore) GetMessages(offset, limit int) ([]Message, error) {
	query := `SELECT timestamp, nick, text, color, ip, mentions FROM messages ORDER BY id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("메시지 조회 실패: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestampStr string
		var mentionsStr sql.NullString
		var ipStr sql.NullString

		err := rows.Scan(&timestampStr, &msg.Nick, &msg.Text, &msg.Color, &ipStr, &mentionsStr)
		if err != nil {
			return nil, fmt.Errorf("메시지 스캔 실패: %w", err)
		}

		// SQLite는 time.Time을 RFC3339 형식의 문자열로 저장합니다.
		// 먼저 RFC3339 형식으로 파싱을 시도합니다.
		parsedTime, err := time.Parse(time.RFC3339, timestampStr)
		if err != nil {
			// RFC3339 파싱에 실패하면 이전 형식으로 시도합니다.
			parsedTime, err = time.Parse("2006-01-02 15:04:05", timestampStr)
			if err != nil {
				log.Printf("경고: 타임스탬프 파싱 실패 (%s): %v", timestampStr, err)
				// 파싱에 실패하면 msg.Time은 time.Time의 제로 값으로 유지됩니다.
				// time.Now()로 설정하지 않아 시간이 계속 업데이트되는 버그를 방지합니다.
			}
		}
		msg.Time = parsedTime

		if ipStr.Valid {
			msg.IP = ipStr.String
		}
		if mentionsStr.Valid && mentionsStr.String != "" {
			msg.Mentions = splitMentions(mentionsStr.String)
		}

		messages = append(messages, msg)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("메시지 행 반복 중 오류: %w", err)
	}

	// 최신순으로 가져왔으므로, 오래된 순으로 정렬하여 반환
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages, nil
}

// GetBans는 데이터베이스에서 모든 밴 목록을 조회합니다.
func (s *SQLiteMessageStore) GetBans() ([]*types.Ban, error) {
	query := `SELECT ip, banned_by, reason, timestamp FROM bans`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("밴 목록 조회 실패: %w", err)
	}
	defer rows.Close()

	var bans []*types.Ban
	for rows.Next() {
		var ban types.Ban
		var timestampStr string
		err := rows.Scan(&ban.IP, &ban.BannedBy, &ban.Reason, &timestampStr)
		if err != nil {
			return nil, fmt.Errorf("밴 정보 스캔 실패: %w", err)
		}

		parsedTime, err := time.Parse(time.RFC3339, timestampStr)
		if err != nil {
			log.Printf("경고: 밴 타임스탬프 파싱 실패 (%s): %v", timestampStr, err)
		}
		ban.At = parsedTime
		bans = append(bans, &ban)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("밴 목록 행 반복 중 오류: %w", err)
	}
	return bans, nil
}

// SaveBan은 밴 정보를 데이터베이스에 저장합니다.
func (s *SQLiteMessageStore) SaveBan(ban *types.Ban) error {
	insertSQL := `INSERT OR REPLACE INTO bans(ip, banned_by, reason, timestamp) VALUES(?, ?, ?, ?)`
	_, err := s.db.Exec(insertSQL, ban.IP, ban.BannedBy, ban.Reason, ban.At)
	if err != nil {
		return fmt.Errorf("밴 정보 데이터베이스 저장 실패: %w", err)
	}
	return nil
}

// RemoveBan은 데이터베이스에서 밴 정보를 삭제합니다.
func (s *SQLiteMessageStore) RemoveBan(ban *types.Ban) error {
	deleteSQL := `DELETE FROM bans WHERE ip = ?`
	_, err := s.db.Exec(deleteSQL, ban.IP)
	if err != nil {
		return fmt.Errorf("밴 정보 데이터베이스 삭제 실패: %w", err)
	}
	return nil
}

// GetMessageCount는 총 메시지 개수를 반환합니다.
func (s *SQLiteMessageStore) GetMessageCount() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("메시지 개수 조회 실패: %w", err)
	}
	return count, nil
}

// Close는 데이터베이스 연결을 닫습니다.
func (s *SQLiteMessageStore) Close() error {
	return s.db.Close()
}

// GetUserColor는 사용자의 색상을 조회합니다.
func (s *SQLiteMessageStore) GetUserColor(nick string) (int, error) {
	var color int
	err := s.db.QueryRow("SELECT color FROM users WHERE nick = ?", nick).Scan(&color)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil // 사용자가 없으면 0 (기본값) 반환
		}
		return 0, fmt.Errorf("사용자 색상 조회 실패: %w", err)
	}
	return color, nil
}

// SetUserColor는 사용자의 색상을 업데이트합니다.
func (s *SQLiteMessageStore) SetUserColor(nick string, color int) error {
	_, err := s.db.Exec("UPDATE users SET color = ? WHERE nick = ?", color, nick)
	if err != nil {
		return fmt.Errorf("사용자 색상 업데이트 실패: %w", err)
	}
	return nil
}

// CreateUser는 새로운 사용자를 추가합니다.
func (s *SQLiteMessageStore) CreateUser(nick string, color int) error {
	_, err := s.db.Exec("INSERT INTO users(nick, color) VALUES(?, ?)", nick, color)
	if err != nil {
		return fmt.Errorf("사용자 생성 실패: %w", err)
	}
	return nil
}

// splitMentions는 쉼표로 구분된 멘션 문자열을 슬라이스로 분리합니다.
func splitMentions(mentions string) []string {
	if mentions == "" {
		return nil
	}
	return strings.Split(mentions, ",")
}
