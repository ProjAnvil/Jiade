-- Batch scheduling database
CREATE TABLE batch_job (
  biz_date       VARCHAR(10) PRIMARY KEY,
  type           VARCHAR(32) NOT NULL,
  status         VARCHAR(16) NOT NULL,           -- RUNNING/SUCCEEDED/FAILED
  total_interest DECIMAL(18,2) NOT NULL DEFAULT 0,
  created_at     TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  finished_at    TIMESTAMP NULL
);

CREATE TABLE batch_unit_result (
  biz_date VARCHAR(10) NOT NULL,
  dcn      VARCHAR(16) NOT NULL,
  accounts INT NOT NULL DEFAULT 0,
  interest DECIMAL(18,2) NOT NULL DEFAULT 0,
  status   VARCHAR(16) NOT NULL,                 -- DONE/FAILED
  error    VARCHAR(512) NULL,
  PRIMARY KEY (biz_date, dcn)
);
