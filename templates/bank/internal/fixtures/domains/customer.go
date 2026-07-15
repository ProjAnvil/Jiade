package domains

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/customer/domain"
	"bank/internal/fixtures"
)

// GenCustomers 生成 n 个客户（cust_id=C%07d(j+1)，与 core 账户的 cust_id 编号一致）。
// 20% 对公（j%5==0）。rng 偏移 +10。
func GenCustomers(cfg fixtures.Config, n int) []domain.Customer {
	rng := fixtures.NewRNG(cfg.Seed + 10)
	out := make([]domain.Customer, n)
	for j := 0; j < n; j++ {
		isOrg := j%5 == 0
		c := domain.Customer{
			CustID:        fmt.Sprintf("C%07d", j+1),
			Nationality:   "CN",
			RiskLevel:     rng.Choice(fixtures.RiskLevels),
			KYCStatus:     "passed",
			CreateBizDate: fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate),
		}
		if isOrg {
			c.CustType = domain.CustTypeOrg
			c.Name = orgName(rng)
			c.CertType = "统一社会信用代码"
			c.CertNo = numerify(rng, 18)
		} else {
			c.CustType = domain.CustTypePersonal
			c.Name = rng.Choice(fixtures.Surnames) + rng.Choice(fixtures.GivenNames)
			c.CertType = "身份证"
			c.CertNo = numerify(rng, 18)
			c.Gender = rng.Choice(fixtures.Genders)
			c.Birthday = fixtures.RandomDate(rng, "1950-01-01", "2007-12-31")
		}
		out[j] = c
	}
	return out
}

// GenAccountRels 由 (custID, accountNo) 对生成户主关系。rel_id 确定性。
func GenAccountRels(pairs [][2]string) []domain.AccountRel {
	out := make([]domain.AccountRel, len(pairs))
	for i, p := range pairs {
		out[i] = domain.AccountRel{
			RelID: fmt.Sprintf("R%010d", i+1), CustID: p[0], AccountNo: p[1],
			Role: "主", RelType: "户主",
		}
	}
	return out
}

// WriteCustomers 幂等写 cust_info（先 DELETE 后 INSERT）。
func WriteCustomers(ctx context.Context, db *sql.DB, rows []domain.Customer) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM cust_info"); err != nil {
		return fmt.Errorf("清空 cust_info: %w", err)
	}
	for _, c := range rows {
		var gender, birthday any
		if c.Gender != "" {
			gender = c.Gender
		}
		if c.Birthday != "" {
			birthday = c.Birthday
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO cust_info
			(cust_id,cust_type,name,cert_type,cert_no,gender,birthday,nationality,risk_level,kyc_status,create_biz_date)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			c.CustID, string(c.CustType), c.Name, c.CertType, c.CertNo,
			gender, birthday, c.Nationality, c.RiskLevel, c.KYCStatus, c.CreateBizDate); err != nil {
			return fmt.Errorf("插入 cust_info %s: %w", c.CustID, err)
		}
	}
	return nil
}

// WriteAccountRels 幂等写 cust_account_rel。
func WriteAccountRels(ctx context.Context, db *sql.DB, rels []domain.AccountRel) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM cust_account_rel"); err != nil {
		return err
	}
	for _, r := range rels {
		if _, err := db.ExecContext(ctx, `INSERT INTO cust_account_rel
			(rel_id,cust_id,account_no,role,rel_type) VALUES ($1,$2,$3,$4,$5)`,
			r.RelID, r.CustID, r.AccountNo, r.Role, r.RelType); err != nil {
			return err
		}
	}
	return nil
}

// orgName 生成对公客户名（行业 + "有限公司"）。
func orgName(rng *fixtures.RNG) string {
	return rng.Choice(fixtures.Industries) + rng.Choice(fixtures.CustRegions) + "有限公司"
}

// numerify 生成 n 位数字串（确定性）。
func numerify(rng *fixtures.RNG, n int) string {
	const digits = "0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[rng.IntRange(0, 9)]
	}
	return string(b)
}
