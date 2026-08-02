-- DCN 单元业务库
CREATE TABLE account (
  account_id INT PRIMARY KEY,
  name       VARCHAR(64),
  balance    DECIMAL(18,2) NOT NULL CHECK (balance >= 0)
);

CREATE TABLE journal (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  tx_id      VARCHAR(64) NOT NULL,
  account_id INT NOT NULL,
  direction  VARCHAR(8) NOT NULL,
  amount     DECIMAL(18,2) NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_tx_acct (tx_id, account_id, direction)
);
