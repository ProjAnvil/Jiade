-- ADM global aggregation database
CREATE TABLE event_log (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  tx_id      VARCHAR(64) NOT NULL,
  account_id INT NOT NULL,
  dcn        VARCHAR(16) NOT NULL,
  direction  VARCHAR(8) NOT NULL,
  amount     DECIMAL(18,2) NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_event (tx_id, account_id, direction)
);

CREATE TABLE global_balance (
  account_id INT PRIMARY KEY,
  dcn        VARCHAR(16) NOT NULL,
  balance    DECIMAL(18,2) NOT NULL
);
