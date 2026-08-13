-- El grafo no decide cuántos créditos se acreditan. El campo se completa al
-- consumir el comprobante, después de calcularlo desde su monto PEN.

UPDATE data_fields f
SET required = FALSE, updated_at = NOW()
FROM data_objects o
WHERE o.id = f.object_id
  AND o.key = 'cobros'
  AND f.key = 'creditos';
