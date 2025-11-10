package chat

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite 드라이버
)

// MessageStore는 메시지 저장소에 대한 인터페이스입니다.
type MessageStore interface {
	Init() error
	AppendMessage(msg Message) error
	GetMessages(offset, limit int) ([]Message, error)
	Close() error
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
	log.Println("SQLite 메시지 테이블 초기화 완료.")
	return nil
}

// AppendMessage는 메시지를 데이터베이스에 추가합니다.
func (s *SQLiteMessageStore) AppendMessage(msg Message) error {
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
	_, err := s.db.Exec(insertSQL, msg.Time, msg.Nick, msg.Text, msg.Color, msg.IP, mentions)
	if err != nil {
		return fmt.Errorf("메시지 데이터베이스 저장 실패: %w", err)
	}
	return nil
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

// Close는 데이터베이스 연결을 닫습니다.
func (s *SQLiteMessageStore) Close() error {
	return s.db.Close()
}

// splitMentions는 쉼표로 구분된 멘션 문자열을 슬라이스로 분리합니다.
func splitMentions(mentions string) []string {
	if mentions == "" {
		return nil
	}
	return strings.Split(mentions, ",")
}
