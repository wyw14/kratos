CREATE TABLE courier_queues (
  nid UUID PRIMARY KEY,
  status INTEGER NOT NULL,
  created_at timestamp NOT NULL,
  updated_at timestamp NOT NULL,
  CONSTRAINT courier_queues_nid_fk FOREIGN KEY (nid) REFERENCES networks (id) ON DELETE cascade
);
