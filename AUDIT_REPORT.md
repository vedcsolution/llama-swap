# Informe de Auditoría Técnica - Swap-Laboratories

**Fecha:** 2026-02-18  
**Auditor:** Kilo Code (Debug Mode)  
**Repositorio:** `/home/csolutions_ai/swap-laboratories`  
**Versión Go:** 1.24.0  

---

## 1. Resumen Ejecutivo

### 1.1 Estado General del Proyecto

**Swap-Laboratories** (también conocido como llama-swap) es un proxy inverso diseñado para gestionar modelos de lenguaje (LLMs) con capacidad de intercambio dinámico de modelos en memoria GPU. El proyecto demuestra una arquitectura sólida con buena separación de responsabilidades.

| Aspecto | Evaluación | Score |
|---------|------------|-------|
| Estructura del código | ✅ Excelente | 9/10 |
| Seguridad | ⚠️ Requiere atención | 6/10 |
| Manejo de errores | ✅ Bueno | 8/10 |
| Documentación | ✅ Buena | 8/10 |
| Test Coverage | ✅ Extensivo | 8/10 |

### 1.2 Stack Tecnológico Detectado

#### Backend
- **Lenguaje:** Go 1.24.0 (toolchain go1.24.13)
- **Framework HTTP:** Gin v1.10.0
- **Procesamiento YAML:** gopkg.in/yaml.v3
- **JSON:** tidwall/gjson + tidwall/sjson
- **Eventos:** Sistema custom con patrones publish/subscribe
- **Notificación de archivos:** fsnotify v1.9.0

#### Frontend
- **Framework:** Svelte 5.19.0
- **Bundler:** Vite 6.3.5
- **Lenguaje:** TypeScript 5.8.3
- **Estilos:** TailwindCSS 4.1.8
- **Testing:** Vitest 4.0.18
- **Editor de código:** CodeMirror 6

#### Infraestructura
- **Contenedores:** Docker (Containerfile incluido)
- **Build:** Makefile + GoReleaser
- **CI/CD:** GitHub Actions (implícito por .github/)

---

## 2. Hallazgos Críticos

### 2.1 🔴 CRÍTICO: Potencial Inyección de Comandos

**Ubicación:** [`proxy/process.go:253`](proxy/process.go:253)

```go
p.cmd = exec.CommandContext(cmdContext, args[0], args[1:]...)
```

**Descripción:** El comando se construye a partir de la configuración YAML del usuario. Si la configuración no es validada adecuadamente, un atacante con acceso a la configuración podría inyectar comandos arbitrarios.

**Impacto:** Ejecución de código arbitrario en el servidor.

**Recomendación:**
```go
// Validar que el comando no contenga caracteres peligrosos
func validateCommand(cmd string) error {
    dangerousChars := []string{";", "&&", "||", "|", "`", "$(", "$"}
    for _, char := range dangerousChars {
        if strings.Contains(cmd, char) {
            return fmt.Errorf("comando contiene caracteres no permitidos: %s", char)
        }
    }
    return nil
}
```

---

### 2.2 🔴 CRÍTICO: CORS Permisivo

**Ubicación:** [`proxy/proxymanager.go:281`](proxy/proxymanager.go:281)

```go
c.Header("Access-Control-Allow-Origin", "*")
```

**Descripción:** El servidor configura CORS para permitir cualquier origen (`*`), lo que permite que cualquier sitio web haga peticiones a la API.

**Impacto:** 
- Exposición de datos a sitios maliciosos
- Posible robo de tokens/API keys
- CSRF attacks

**Recomendación:**
```go
// Configurar orígenes permitidos en la configuración
allowedOrigins := pm.config.AllowedOrigins // []string desde config.yaml
origin := c.GetHeader("Origin")
for _, allowed := range allowedOrigins {
    if origin == allowed {
        c.Header("Access-Control-Allow-Origin", origin)
        break
    }
}
```

---

### 2.3 🟠 ALTO: Ejecución de Scripts Shell

**Ubicaciones afectadas:**

| Archivo | Línea | Patrón |
|---------|-------|--------|
| [`proxy/cluster_status_api.go`](proxy/cluster_status_api.go:252) | 252 | `exec.CommandContext(ctx, "bash", "-lc", script)` |
| [`proxy/cluster_dgx_api.go`](proxy/cluster_dgx_api.go:241) | 241 | `exec.CommandContext(ctx, "bash", "-lc", script)` |
| [`proxy/recipes_ui.go`](proxy/recipes_ui.go:368) | 368 | `exec.CommandContext(ctx, "bash", "-lc", trtllmUpdateScript(...))` |

**Descripción:** Múltiples puntos del código ejecutan scripts bash con contenido dinámico. Si bien algunos scripts son internos, el patrón `bash -lc` con strings construidos dinámicamente es riesgoso.

**Recomendación:** 
1. Usar `exec.Command` con argumentos separados en lugar de strings concatenados
2. Implementar lista blanca de comandos permitidos
3. Sanitizar todas las entradas antes de pasarlas a shell

---

### 2.4 🟠 ALTO: API Keys en Memoria y Logs

**Ubicación:** [`proxy/proxymanager.go:982-987`](proxy/proxymanager.go:982)

```go
for _, key := range pm.config.RequiredAPIKeys {
    if providedKey == key {
        valid = true
        break
    }
}
```

**Descripción:** Las API keys se almacenan en memoria como texto plano y se comparan de forma insegura (timing attack potencial).

**Impacto:** 
- Timing attacks para determinar keys válidas
- Exposición en dumps de memoria
- Posible logging accidental

**Recomendación:**
```go
import "crypto/subtle"

// Usar comparación de tiempo constante
valid := subtle.ConstantTimeCompare([]byte(providedKey), []byte(expectedKey)) == 1
```

---

### 2.5 🟡 MEDIO: Sin Rate Limiting

**Ubicación:** [`proxy/proxymanager.go`](proxy/proxymanager.go) - Toda la API

**Descripción:** No se implementa rate limiting en los endpoints de autenticación, permitiendo ataques de fuerza bruta contra las API keys.

**Recomendación:** Implementar rate limiting con middleware:
```go
import "golang.org/x/time/rate"

func rateLimiter() gin.HandlerFunc {
    limiter := rate.NewLimiter(rate.Every(time.Second), 10) // 10 req/seg
    return func(c *gin.Context) {
        if !limiter.Allow() {
            c.AbortWithStatus(http.StatusTooManyRequests)
            return
        }
        c.Next()
    }
}
```

---

## 3. Análisis de Calidad

### 3.1 Problemas de Lógica

#### 3.1.1 Race Condition en State Management

**Ubicación:** [`proxy/process.go:157-182`](proxy/process.go:157)

```go
func (p *Process) swapState(expectedState, newState ProcessState) (ProcessState, error) {
    p.stateMutex.Lock()
    defer p.stateMutex.Unlock()
    // ...
}
```

**Análisis:** El código implementa correctamente el patrón de compare-and-swap con mutex. Sin embargo, hay una inconsistencia en [`process.go:384-406`](process.go:384) donde se usa un bucle de reintento que podría causar livelock bajo alta contención.

---

#### 3.1.2 Memory Leak Potencial en Event Dispatcher

**Ubicación:** [`event/event.go:286-298`](event/event.go:286)

```go
func (s *group[T]) Del(sub *consumer[T]) {
    s.cond.L.Lock()
    defer s.cond.L.Unlock()
    sub.stop = true
    for i, v := range s.subs {
        if v == sub {
            copy(s.subs[i:], s.subs[i+1:])
            s.subs = s.subs[:len(s.subs)-1]
            break
        }
    }
}
```

**Problema:** No se notifica al `sync.Cond` después de eliminar un subscriber, lo que podría dejar goroutines bloqueadas esperando.

---

#### 3.1.3 Goroutine Leak en Health Check

**Ubicación:** [`proxy/process.go:339-358`](proxy/process.go:339)

```go
go func() {
    maxDuration := time.Duration(p.config.UnloadAfter) * time.Second
    for range time.Tick(time.Second) {
        // ...
    }
}()
```

**Problema:** La goroutine del TTL checker no tiene mecanismo de cancelación explícito. Depende únicamente del estado del proceso, lo que podría causar goroutines huérfanas.

**Recomendación:** Usar `context.Context` para cancelación:
```go
go func() {
    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // check logic
        }
    }
}()
```

---

### 3.2 Eficiencia y Optimización

#### 3.2.1 Asignación Innecesaria en Proxy Request

**Ubicación:** [`proxy/proxymanager.go:642`](proxy/proxymanager.go:642)

```go
bodyBytes, err := io.ReadAll(c.Request.Body)
```

**Problema:** Se lee todo el body en memoria antes de procesar. Para requests grandes (archivos multimedia, etc.), esto puede causar presión de memoria significativa.

**Recomendación:** Usar streaming para bodies grandes:
```go
// Limitar tamaño máximo
limitedReader := io.LimitReader(c.Request.Body, maxBodySize)
bodyBytes, err := io.ReadAll(limitedReader)
```

---

#### 3.2.2 Mapa Sin Límite de Crecimiento

**Ubicación:** [`proxy/proxymanager.go:186-187`](proxy/proxymanager.go:186)

```go
benchyJobs:    make(map[string]*BenchyJob),
benchyCancels: make(map[string]context.CancelFunc),
```

**Problema:** Los mapas de benchy jobs no tienen límite de tamaño ni limpieza periódica, causando potencial memory leak en uso prolongado.

---

### 3.3 Adherencia a Estándares

#### 3.3.1 ✅ Buenas Prácticas Detectadas

1. **Context Usage:** Uso extensivo de `context.Context` para cancelación
2. **Error Handling:** Manejo de errores consistente con wrapping
3. **Logging:** Sistema de logging estructurado con niveles
4. **Testing:** Cobertura extensa con testify
5. **Documentation:** Comentarios godoc en funciones públicas

#### 3.3.2 ⚠️ Áreas de Mejora

1. **Error Messages:** Algunos mensajes de error no incluyen contexto suficiente
2. **Magic Numbers:** Constantes numéricas sin nombre (ej: `32 << 20` en línea 770)
3. **Interface Segregation:** Algunas interfaces son demasiado grandes

---

## 4. Vulnerabilidades de Seguridad Detalladas

### 4.1 Matriz de Riesgo

| Vulnerabilidad | Severidad | Explotabilidad | Impacto | Prioridad |
|----------------|-----------|----------------|---------|-----------|
| Inyección de comandos | Crítica | Media | Alto | P1 |
| CORS permisivo | Alta | Alta | Medio | P1 |
| API keys en memoria | Alta | Baja | Alto | P2 |
| Sin rate limiting | Media | Alta | Medio | P2 |
| Scripts shell dinámicos | Alta | Media | Alto | P1 |

### 4.2 Flujo de Datos Sensibles

```
[Cliente] → API Key → [ProxyManager] → Validación → [Memoria]
                              ↓
                        Headers removidos ✅
                              ↓
                        [Upstream] (sin API key)
```

**Observación:** El sistema correctamente remueve los headers de autenticación antes de enviar al upstream (línea 1001-1002 de [`proxymanager.go`](proxy/proxymanager.go:1001)).

### 4.3 Headers Sensibles Redactados

**Ubicación:** [`proxy/metrics_monitor.go:482-484`](proxy/metrics_monitor.go:482)

```go
var sensitiveHeaders = map[string]bool{
    "authorization":       true,
    "set-cookie":          true,
    "x-api-key":           true,
}
```

✅ **Buen práctica:** Los headers sensibles son redactados en los logs de métricas.

---

## 5. Recomendaciones

### 5.1 Acciones Inmediatas (P1 - Dentro de 1 semana)

1. **Corregir CORS**
   - Implementar lista de orígenes permitidos configurable
   - Validar origen contra whitelist antes de establecer headers

2. **Validación de Comandos**
   - Implementar sanitización estricta de comandos en configuración
   - Crear whitelist de ejecutables permitidos

3. **Auditoría de Scripts Shell**
   - Revisar todos los puntos de ejecución de bash
   - Migrar a exec.Command con argumentos separados

### 5.2 Acciones a Corto Plazo (P2 - Dentro de 1 mes)

4. **Implementar Rate Limiting**
   - Añadir middleware de rate limiting en endpoints de autenticación
   - Considerar rate limiting por IP y por API key

5. **Mejorar Gestión de Secrets**
   - Migrar a comparación de tiempo constante para API keys
   - Considerar uso de hash para almacenamiento de keys

6. **Corregir Memory Leaks**
   - Implementar cleanup periódico de benchy jobs
   - Añadir cancelación por contexto en goroutines de TTL

### 5.3 Acciones a Mediano Plazo (P3 - Dentro de 3 meses)

7. **Implementar Content Security Policy**
   - Añadir headers CSP en respuestas HTTP
   - Configurar políticas restrictivas para UI

8. **Auditoría de Dependencias**
   - Ejecutar `go mod` con verificaciçon de checksums
   - Implementar renovación periódica de dependencias

9. **Mejorar Observabilidad de Seguridad**
   - Añadir logging de eventos de seguridad
   - Implementar métricas de intentos de autenticación fallidos

### 5.4 Mejoras de Código Específicas

#### Corrección para [`proxy/process.go:643`](proxy/process.go:643)

```go
// ANTES
stopCmd := exec.Command(stopArgs[0], stopArgs[1:]...)

// DESPUÉS - Validar comando antes de ejecutar
if err := validateExecutable(stopArgs[0]); err != nil {
    return fmt.Errorf("invalid stop command: %w", err)
}
stopCmd := exec.Command(stopArgs[0], stopArgs[1:]...)
```

#### Corrección para CORS en [`proxy/proxymanager.go`](proxy/proxymanager.go)

```go
// Añadir a Config struct
type Config struct {
    // ... existing fields
    AllowedOrigins []string `yaml:"allowedOrigins"`
}

// Middleware CORS mejorado
func (pm *ProxyManager) corsMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        if c.Request.Method == "OPTIONS" {
            origin := c.GetHeader("Origin")
            if pm.isOriginAllowed(origin) {
                c.Header("Access-Control-Allow-Origin", origin)
            }
            // ... resto de headers CORS
        }
    }
}
```

---

## 6. Conclusión

**Swap-Laboratories** es un proyecto bien estructurado con una arquitectura sólida para su propósito. El código demuestra buenas prácticas en cuanto a concurrencia, manejo de errores y testing. Sin embargo, existen vulnerabilidades de seguridad significativas que deben abordarse antes de un despliegue en producción, particularmente relacionadas con:

1. **Configuración CORS permisiva** - Riesgo de exposición de datos
2. **Ejecución de comandos** - Potencial para inyección
3. **Gestión de secrets** - Comparaciones inseguras

### Score Final de Seguridad: 6.5/10

El proyecto es apto para uso interno/desarrollo pero requiere las correcciones P1 antes de exponerse a redes no confiables.

---

**Fin del Informe**

*Generado por Kilo Code - Debug Mode*
