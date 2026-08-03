-- La persistencia especializada se retiró durante desarrollo. Los cobros se
-- guardan mediante data_mutate en data_records y no requieren una tabla paralela.
DROP TABLE IF EXISTS payment_receipts;
