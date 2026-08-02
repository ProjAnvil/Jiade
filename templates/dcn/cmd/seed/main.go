// seed 经 GNS 全流程开户灌入确定性测试数据（仿真生产的路由注册）。
// jiade CLI 硬编码调用：go run ./cmd/seed --scale=<dev|full> [--reset]
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

var (
	scale = flag.String("scale", "dev", "dev|full")
	reset = flag.Bool("reset", false, "clear all business data before seeding")
)

var surnames = []string{"赵", "钱", "孙", "李", "周", "吴", "郑", "王", "冯", "陈", "褚", "卫", "蒋", "沈", "韩", "杨"}

var givenNames = []string{"伟", "芳", "娜", "敏", "静", "磊", "军", "洋", "勇", "艳", "杰", "娟", "涛", "明", "超", "秀兰", "霞", "平", "刚", "桂英"}

// personName 用词汇表拼中文姓名。
func personName(r *rand.Rand) string {
	return surnames[r.Intn(len(surnames))] + givenNames[r.Intn(len(givenNames))]
}

// initialBalance 每单元前 2 户固定 1000.00（verify/README 依赖），其余 100–100000 随机。
func initialBalance(r *rand.Rand, seg, i int) string {
	if i < 2 {
		return "1000.00"
	}
	cents := 10000 + r.Int63n(9990000) // 100.00 ~ 100000.00（单位：分）
	return fmt.Sprintf("%.2f", float64(cents)/100)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	flag.Parse()
	counts := map[string]int{"dev": 50, "full": 2000}
	n, ok := counts[*scale]
	if !ok {
		log.Fatalf("unknown scale %q (want dev|full)", *scale)
	}
	if *reset {
		resetAll()
	}
	gns := envOr("GNS_ENDPOINT", "http://localhost:18080")
	hc := &http.Client{Timeout: 10 * time.Second}
	for _, seg := range []int{1000, 2000, 3000} {
		r := rand.New(rand.NewSource(int64(seg))) // 每单元确定性
		for i := 0; i < n; i++ {
			name := personName(r)
			bal := initialBalance(r, seg, i)
			body, _ := json.Marshal(map[string]string{
				"name":        name,
				"initBalance": bal,
				"requestId":   fmt.Sprintf("seed-%s-%d-%d", *scale, seg, i), // 幂等键
			})
			resp, err := hc.Post(gns+"/accounts", "application/json", bytes.NewReader(body))
			if err != nil {
				log.Fatalf("open account via GNS: %v", err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				log.Fatalf("GNS returned %d: %s", resp.StatusCode, raw)
			}
			fmt.Printf("seeded: %s\n", raw)
		}
	}
	fmt.Println("seed done")
}

// resetAll 清空业务数据（保留 route_segment），并清 GNS 路由缓存。
func resetAll() {
	dbs := map[string][]string{
		envOr("SEED_DSN_GNS", "root:dcn123@tcp(127.0.0.1:13309)/gns_db"):     {"account_route"},
		envOr("SEED_DSN_RMB", "root:dcn123@tcp(127.0.0.1:13310)/rmb_db"):     {"tx_step_log", "tx_log"},
		envOr("SEED_DSN_ADM", "root:dcn123@tcp(127.0.0.1:13311)/adm_db"):     {"event_log", "global_balance"},
		envOr("SEED_DSN_DCN01", "root:dcn123@tcp(127.0.0.1:13306)/dcn01_db"): {"journal", "account"},
		envOr("SEED_DSN_DCN02", "root:dcn123@tcp(127.0.0.1:13307)/dcn02_db"): {"journal", "account"},
		envOr("SEED_DSN_DCN03", "root:dcn123@tcp(127.0.0.1:13308)/dcn03_db"): {"journal", "account"},
		envOr("SEED_DSN_BATCH", "root:dcn123@tcp(127.0.0.1:13313)/batch_db"): {"batch_unit_result", "batch_job"},
	}
	for dsn, tables := range dbs {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			log.Fatalf("open %s: %v", dsn, err)
		}
		for _, t := range tables {
			if _, err := db.Exec("DELETE FROM " + t); err != nil {
				log.Fatalf("clear %s: %v", t, err)
			}
		}
		db.Close()
	}
	rdb := redis.NewClient(&redis.Options{Addr: envOr("SEED_REDIS_ADDR", "127.0.0.1:16379")})
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		log.Printf("warn: flush gns redis: %v", err)
	}
	fmt.Println("reset done")
}
