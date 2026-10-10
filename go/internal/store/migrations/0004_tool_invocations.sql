-- 0004 · Una invocación de tool es un hecho registrado, aunque no haya run (INT-013).
--
-- INT-011 estableció que un desenlace DENEGADO no puede ser un silencio, y lo resolvió escribiendo
-- en el journal del run. Esta migración es su complemento, y existe porque ese journal deja dos
-- silencios que se midieron:
--
--   1. UNA EJECUCIÓN QUE SÍ OCURRE no se registra en ninguna puerta. El journal del run recoge
--      denegaciones; de las llamadas que se ejecutan no queda nada.
--   2. UNA LLAMADA SIN RUN no tiene dónde registrarse. `journalDenial` lo dice con sus palabras
--      —"no run_id/step_id on the request: there is no journal to record this against"— y todo
--      llamador MCP externo está en ese caso por diseño (INT-003: no es un run).
--
-- POR QUÉ NO ES `tool_executions`, que ya existe y guarda args y result. Esa tabla es la
-- RECLAMACIÓN de idempotencia (TOOL-005): su clave primaria es la clave de idempotencia, así que
-- una llamada sin clave no tiene fila posible, y su cardinalidad es una fila POR CLAVE. Un replay
-- servido desde su caché responde `deduplicated: true` y no escribe nada, de modo que la tabla no
-- puede contar los reintentos ni siquiera en principio. Esta es append-only, una fila POR INTENTO.
-- Son dos hechos distintos: una es un cerrojo con caché de resultado, la otra es la bitácora.
--
-- NO HAY COLUMNA DE COSTO, y es deliberado. Ningún tool del repo tiene precio hoy: la columna
-- quedaría NULL en el 100% de las filas y sería el cuarto campo declarado que nadie produce —
-- justo el defecto que venimos reportando a otros equipos (`mcp_origin` lleva así desde F0). El
-- costo entra cuando exista un productor: una fuente MCP federada que informe lo que cobra.
CREATE TABLE IF NOT EXISTS tool_invocations (
    id                 BIGSERIAL PRIMARY KEY,
    tenant_id          TEXT NOT NULL,
    tool_name          TEXT NOT NULL,
    -- La puerta por la que entró. No es cosmética: el hallazgo que motivó esta tabla es que el
    -- registro dependía de la puerta, y sin esta columna una auditoría no puede responder "¿por
    -- dónde entró esto?" ni detectar que una puerta nueva no registra.
    door               TEXT NOT NULL,
    outcome            TEXT NOT NULL,
    -- NULL cuando el desenlace no fue error. Tres estados y no dos: cadena vacía significaría
    -- "falló sin mensaje", que es un hecho distinto de "no falló".
    error_message      TEXT,
    -- Cadena vacía = no había run, que es una respuesta real y no un dato ausente: un llamador MCP
    -- externo no es un run. Se guarda '' en vez de NULL porque la ausencia aquí es estructural y
    -- constante para esa puerta, no desconocimiento.
    run_id             TEXT NOT NULL DEFAULT '',
    step_id            TEXT NOT NULL DEFAULT '',
    agent_manifest_ref TEXT NOT NULL DEFAULT '',
    -- NULL solo si algún día se registra algo que no se ejecutó. Hoy toda fila la escribe Execute
    -- después de ejecutar, así que siempre lleva duración medida.
    duration_ms        BIGINT,
    occurred_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tool_invocations_outcome_valid CHECK (outcome IN ('ok', 'error')),
    -- Un mensaje de error sin error, o un error sin mensaje, son filas que mienten en una
    -- auditoría. La base se niega a guardarlas en vez de confiar en que cada llamador acierte.
    CONSTRAINT tool_invocations_error_message_iff_error CHECK (
        (outcome = 'error' AND error_message IS NOT NULL) OR
        (outcome <> 'error' AND error_message IS NULL)
    )
);

-- Las dos preguntas que una auditoría hace de verdad: qué hizo este run, y quién llamó a este tool.
CREATE INDEX IF NOT EXISTS tool_invocations_tenant_run_idx
    ON tool_invocations (tenant_id, run_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS tool_invocations_tenant_tool_idx
    ON tool_invocations (tenant_id, tool_name, occurred_at DESC);
