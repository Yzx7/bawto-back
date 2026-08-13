-- La clasificación visual deja de decidir si un cobro es válido. Conservamos
-- el texto que la imagen muestra y el servidor determina si expresa éxito.

INSERT INTO data_fields(object_id, key, label, type, required)
SELECT o.id, 'resultado', 'Resultado visible', 'text', FALSE
FROM data_objects o
WHERE o.key = 'cobros'
ON CONFLICT (object_id, key) DO UPDATE
SET label = EXCLUDED.label,
    type = EXCLUDED.type,
    required = EXCLUDED.required,
    updated_at = NOW();
