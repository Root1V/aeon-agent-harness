# Fixtures dorados — costura de durabilidad

Cada caso es una secuencia de operaciones sobre un `run_id` **limpio** y el resultado que cualquier
implementación correcta tiene que reproducir.

**Cómo comparar:**

- En `append`, se comparan los tres campos del resultado tal cual.
- En `load`, se comparan `next_seq` y, para cada record en orden, la proyección
  `(step_id, phase, seq, payload)`. **`recorded_at` se ignora**: es un hecho del reloj, no del
  contrato.
- En `query`, se comparan las consultas derivadas del estado cargado.
- `expect_error` significa que la operación se rechaza antes de escribir nada. El código o el tipo
  de error no son parte de este contrato todavía; sí lo es que **no escriba**.

Cada caso lleva un `why`. Si un caso se rompe y el `why` ya no describe nada que importe, el caso
sobra — bórralo en vez de ajustarlo hasta que pase.

**Runner:** lo trae cada proyecto. Aquí no entra implementación.
