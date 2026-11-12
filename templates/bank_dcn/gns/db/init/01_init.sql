-- GNS global routing database
CREATE TABLE route_segment (
  dcn        VARCHAR(16) PRIMARY KEY,
  seg_start  INT NOT NULL,
  seg_end    INT NOT NULL,
  endpoint   VARCHAR(128) NOT NULL,
  status     VARCHAR(16) NOT NULL
);

CREATE TABLE account_route (
  account_id INT PRIMARY KEY,
  dcn        VARCHAR(16) NOT NULL,
  request_id VARCHAR(64) UNIQUE,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO route_segment (dcn, seg_start, seg_end, endpoint, status) VALUES
  ('dcn01', 1000, 1999, 'http://dcn01-app:8080', 'ACTIVE'),
  ('dcn02', 2000, 2999, 'http://dcn02-app:8080', 'ACTIVE'),
  ('dcn03', 3000, 3999, 'http://dcn03-app:8080', 'ACTIVE');
