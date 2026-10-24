CREATE TABLE courier_queues (
  nid CHAR(36) PRIMARY KEY,
  status INTEGER NOT NULL,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  CONSTRAINT courier_queues_nid_fk FOREIGN KEY (nid) REFERENCES networks (id) ON DELETE cascade
) ENGINE=InnoDB;
