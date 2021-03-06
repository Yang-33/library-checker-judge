package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/lib/pq"
)

// Problem is db table
type Problem struct {
	Name      string
	Title     string
	Statement string
	Timelimit int32
	Testhash  string
}

// User is db table
type User struct {
	Name     string
	Passhash string
	Admin    bool
}

// Submission is db table
type Submission struct {
	ID          int32
	ProblemName string
	Problem     Problem `gorm:"foreignkey:ProblemName"`
	Lang        string
	Status      string
	PrevStatus  string
	Hacked      bool
	Source      string
	Testhash    string
	MaxTime     int32
	MaxMemory   int64
	JudgePing   time.Time
	JudgeName   string
	JudgeTasked bool
	UserName    sql.NullString
	User        User `gorm:"foreignkey:UserName"`
}

// SubmissionTestcaseResult is db table
type SubmissionTestcaseResult struct {
	Submission int32
	Testcase   string
	Status     string
	Time       int32
	Memory     int64
}

// Task is db table
type Task struct {
	ID         int32
	Submission int32
	Priority   int32
	Available  time.Time
}

func fetchSubmission(id int32) (Submission, error) {
	sub := Submission{}
	if err := db.
		Preload("User", func(db *gorm.DB) *gorm.DB {
			return db.Select("name")
		}).
		Preload("Problem", func(db *gorm.DB) *gorm.DB {
			return db.Select("name, title, testhash")
		}).
		Where("id = ?", id).First(&sub).Error; err != nil {
		return Submission{}, errors.New("Submission fetch failed")
	}
	return sub, nil
}

func pushTask(task Task) error {
	log.Print("Insert task:", task)
	if err := db.Create(&task).Error; err != nil {
		log.Print(err)
		return errors.New("Cannot insert into queue")
	}
	return nil
}

func clearAllTasks() error {
	for {
		task := Task{}
		if err := db.Take(&task).Error; gorm.IsRecordNotFoundError(err) {
			return nil
		}
		if err := db.Delete(task).Error; err != nil {
			return err
		}
	}
}

func popTask() (Task, error) {
	tx := db.Begin()
	task := Task{}
	err := tx.Set("gorm:query_option", "FOR UPDATE").Where("available <= ?", time.Now()).Order("priority desc").First(&task).Error
	if gorm.IsRecordNotFoundError(err) {
		return Task{Submission: -1}, tx.Rollback().Error
	}
	if err != nil {
		log.Print(err)
		if err := tx.Rollback().Error; err != nil {
			log.Print(err)
		}
		return Task{}, errors.New("Connection to db failed")
	}
	if tx.Delete(task).RowsAffected != 1 {
		log.Print("Failed to delete task:", task.ID)
		return Task{Submission: -1}, tx.Rollback().Error
	}
	if err := tx.Commit().Error; err != nil {
		log.Print(err)
		return Task{}, errors.New("Commit to db failed")
	}
	return task, nil
}

func toWaitingJudge(id int32, priority int32, after time.Duration) error {
	sub, err := fetchSubmission(id)
	if err != nil {
		return err
	}
	if err := db.Model(&Submission{
		ID: id,
	}).Updates(map[string]interface{}{
		"prev_status": sub.Status,
		"judge_name":  "#dummy",
		"judge_ping":  time.UnixDate,
	}).Error; err != nil {
		log.Print(err)
		return errors.New("Failed to clear judge_name")
	}
	if err := pushTask(Task{
		Submission: id,
		Available:  time.Now().Add(after),
		Priority:   priority,
	}); err != nil {
		return errors.New("Cannot insert into queue")
	}
	return nil
}

type RegistrationStatus int

const (
	Undefined RegistrationStatus = iota
	Register                     // WJに自分が登録
	Update                       // 自分のRegisterを延長
	OverWrite                    // 他がジャッジだけど放置されてたので上書き
	Occupied                     // 他がジャッジ中
	Finished                     // WJではない
)

func (status RegistrationStatus) String() string {
	switch status {
	case Undefined:
		return "Undefined"
	case Register:
		return "Register"
	case Update:
		return "Update"
	case OverWrite:
		return "OverWrite"
	case Occupied:
		return "Occupied"
	case Finished:
		return "Finished"
	default:
		return "Unknown"
	}
}

func updateSubmissionRegistration(id int32, judgeName string, expiration time.Duration) (RegistrationStatus, error) {
	tx := db.Begin()
	sub := Submission{}
	if err := tx.Set("gorm:query_option", "FOR UPDATE").Take(&sub, id).Error; err != nil {
		tx.Rollback()
		log.Print(err)
		return Undefined, errors.New("Submission fetch failed")
	}
	if sub.JudgeName == "" {
		return Finished, tx.Rollback().Error
	}
	now := time.Now()
	registered := sub.JudgeName != "" && sub.JudgePing.After(now)
	if registered && sub.JudgeName != judgeName {
		return Occupied, tx.Rollback().Error
	}
	myself := registered && sub.JudgeName == judgeName
	if err := tx.Model(&sub).Updates(map[string]interface{}{
		"judge_name": judgeName,
		"judge_ping": now.Add(expiration),
	}).Error; err != nil {
		log.Print(err)
		if tx.Rollback().Error != nil {
			log.Print("rollback error")
		}
		return Undefined, errors.New("Submission update failed")
	}
	if err := tx.Commit().Error; err != nil {
		log.Print(err)
		return Undefined, errors.New("Transaction commit failed")
	}
	if myself {
		return Update, nil
	}
	return Register, nil
}

func dbConnect() *gorm.DB {
	host := getEnv("POSTGRE_HOST", "127.0.0.1")
	port := getEnv("POSTGRE_PORT", "5432")
	user := getEnv("POSTGRE_USER", "postgres")
	pass := getEnv("POSTGRE_PASS", "passwd")

	connStr := fmt.Sprintf(
		"host=%s port=%s user=%s dbname=librarychecker password=%s sslmode=disable",
		host, port, user, pass)
	log.Printf("Try connect %s", connStr)
	for i := 0; i < 3; i++ {
		db, err := gorm.Open("postgres", connStr)
		if err != nil {
			log.Printf("Cannot connect db %d/3", i)
			time.Sleep(5 * time.Second)
			continue
		}
		db.AutoMigrate(Problem{})
		db.AutoMigrate(User{})
		db.AutoMigrate(Submission{})
		db.AutoMigrate(SubmissionTestcaseResult{})
		db.AutoMigrate(Task{})
		db.BlockGlobalUpdate(true)
		db.DB().SetMaxOpenConns(10)
		db.DB().SetConnMaxLifetime(time.Hour)
		return db
	}
	log.Fatal("Cannot connect db 3 times")
	return nil
}
