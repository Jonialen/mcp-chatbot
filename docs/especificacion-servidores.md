# Especificación de los servidores MCP desarrollados

Proyecto 1 — CC3067 Redes, Universidad del Valle de Guatemala.

Este documento describe los dos servidores MCP implementados para el proyecto:
su transporte, su ciclo de vida, sus métodos, y los parámetros de cada
herramienta que publican. Los esquemas y los frames que aparecen aquí fueron
capturados de ejecuciones reales de los servidores, no redactados a mano.

| Servidor | Repositorio | Transporte | Herramientas |
| --- | --- | --- | --- |
| **BrewOps** | [Jonialen/brewops-mcp](https://github.com/Jonialen/brewops-mcp) | stdio | 9 |
| **netprobe** | [Jonialen/mcp-chatbot](https://github.com/Jonialen/mcp-chatbot) (`cmd/netprobe`) | Streamable HTTP | 3 |

Ambos implementan el protocolo directamente sobre JSON-RPC 2.0, sin utilizar un
SDK de MCP.

---

## 1. Fundamento común: JSON-RPC 2.0

MCP no define un formato de mensajes propio: utiliza JSON-RPC 2.0 en la capa de
aplicación. Los tres tipos de mensaje comparten una misma forma en el cable y se
distinguen únicamente por qué campos están presentes.

| Tipo | `method` | `id` | Se responde |
| --- | --- | --- | --- |
| Petición | sí | sí | sí |
| Notificación | sí | **no** | **nunca** |
| Respuesta | no | sí | — |

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": { "name": "scale_recipe", "arguments": { } }
}
```

Un decodificador no puede saber de antemano qué tipo de mensaje ha llegado, por
lo que la clasificación ocurre después de deserializar. Responder a una
notificación es una violación del protocolo, no una cortesía.

### 1.1 Códigos de error

Se utilizan los códigos estándar de JSON-RPC 2.0:

| Código | Significado | Cuándo lo emiten estos servidores |
| --- | --- | --- |
| `-32700` | Parse error | Un frame que no es JSON válido. La respuesta lleva `id: null`, porque no había id que leer. |
| `-32600` | Invalid Request | Reservado. |
| `-32601` | Method not found | Un método que el servidor no implementa. |
| `-32602` | Invalid params | Parámetros malformados, o una herramienta que el servidor nunca publicó. |
| `-32603` | Internal error | Fallo al serializar un resultado. |

### 1.2 Distinción crítica: fallo de protocolo contra fallo de herramienta

Esta distinción es la decisión de diseño más importante del protocolo y ambos
servidores la respetan.

| Situación | Cómo viaja |
| --- | --- |
| Método desconocido, parámetros inválidos, herramienta inexistente | Objeto `error` de JSON-RPC |
| **Una herramienta se ejecutó y falló** | **Respuesta exitosa con `result.isError: true`** |

El motivo no es estilístico. El fallo de una herramienta está dirigido al
**modelo**, para que lo lea y elija otro camino. Si viajara como error de
protocolo, el cliente lo trataría como excepción y la conversación terminaría
por un problema que el modelo podía haber resuelto.

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "result": {
    "content": [{"type": "text", "text": "no coffee in the catalogue matches \"Sumatra\""}],
    "isError": true
  }
}
```

### 1.3 Ciclo de vida

El handshake son **tres** mensajes, no dos:

```
cliente → servidor    initialize                      (petición)
cliente ← servidor    result: protocolVersion, ...    (respuesta)
cliente → servidor    notifications/initialized       (notificación)
```

El tercer mensaje no es opcional: un servidor puede rechazar peticiones hasta
recibirlo, y omitirlo produce una sesión que parece conectada y no responde.

**Negociación de versión.** Ambos servidores devuelven la versión que el cliente
solicitó cuando pueden hablarla, en lugar de imponer la suya. Un cliente
construido contra una revisión anterior habla una superficie de herramientas que
no ha cambiado, y rechazarlo por el número costaría interoperabilidad sin ganar
nada.

### 1.4 Métodos implementados

| Método | Tipo | Descripción |
| --- | --- | --- |
| `initialize` | petición | Abre la sesión y negocia versión y capacidades |
| `notifications/initialized` | notificación | El cliente confirma que está listo |
| `tools/list` | petición | Devuelve las herramientas publicadas y sus esquemas |
| `tools/call` | petición | Invoca una herramienta |

Las superficies `resources/` y `prompts/` no se implementan. Ambos servidores
declaran únicamente la capacidad `tools`.

---

## 2. BrewOps

Servidor de conocimiento de una cafetería de especialidad: catálogo, recetas,
perfiles de tueste y registro de preparaciones.

Su razón de existir es que un modelo de lenguaje no tenga que inventar números.
Ante la petición de escalar una receta, un modelo produce cifras que *parecen*
correctas; una cafetería que necesita la misma taza dos veces no puede usar
cifras que parecen correctas. Todas las herramientas calculan a partir de los
registros del negocio.

### 2.1 Transporte y ejecución

| | |
| --- | --- |
| Transporte | stdio, JSON-RPC delimitado por saltos de línea |
| Invocación | `brewops [-db ruta]` |
| Entrada | frames en `stdin` |
| Salida | frames en `stdout`, **exclusivamente** |
| Diagnóstico | `stderr` |
| Persistencia | SQLite (`modernc.org/sqlite`, Go puro, sin cgo) |
| Versión de protocolo | `2025-06-18` |

`stdout` transporta frames y nada más. Una sola línea ajena en ese flujo corrompe
el stream para el cliente, razón por la cual todo diagnóstico va a `stderr`.

**Instalación:**

```sh
go install github.com/Jonialen/brewops-mcp@latest
```

**Configuración en un anfitrión:**

```json
{
  "mcpServers": {
    "brewops": {
      "command": "/ruta/absoluta/a/brewops",
      "args": ["-db", "./brewops.db"]
    }
  }
}
```

En el primer arranque crea y siembra la base de datos, de modo que las
herramientas tienen con qué responder de inmediato.

### 2.2 Herramientas

#### `compare_roast_batches`

Compare two roast batches of the same coffee landmark by landmark, grade how significant each difference is, and name the change most likely to explain a difference in the cup. Use this when a roaster reports that a new batch of a familiar coffee is tasting different.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `batch_a` | string | sí | The earlier batch label, used as the reference. |
| `batch_b` | string | sí | The later batch label, the one being questioned. |
| `coffee` | string | sí | Coffee name, whole or partial. |

#### `diagnose_extraction`

Compare a brew that was actually made against the shop's recipe, report every variable that fell outside its window, and recommend the single change to make next. Use this whenever a barista describes a brew that did not come out as expected. The recommendation deliberately changes one variable at a time, because changing two makes the next cup impossible to interpret.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `coffee` | string | sí | Coffee name, whole or partial. |
| `dose_grams` | number | sí | Grams of coffee used. |
| `grind_label` | string | no | Grind setting used, if noted. |
| `method` | string | sí | Brewing method. |
| `seconds` | integer | sí | How long the extraction took, in seconds. |
| `taste_notes` | string | no | What the barista tasted, in their own words. |
| `tds` | number | no | Refractometer reading as a percentage, if one was taken. Optional; supplying it adds an extraction yield. |
| `temp_c` | number | no | Water temperature in Celsius, if it was measured. |
| `water_grams` | number | sí | Grams of water used. |

#### `get_coffee`

Look one coffee up by name and return everything the shop knows about it, including which methods it has recipes for. The name may be partial: 'Guji' finds 'Ethiopia Guji Uraga'.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `name` | string | sí | Coffee name, whole or partial. |

#### `get_recipe`

Return the shop's recorded recipe for one coffee on one brewing method: ratio, water temperature, grind, bloom and the expected extraction window. Use this instead of recalling a general recipe, because these are the numbers this shop has settled on for this lot.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `coffee` | string | sí | Coffee name, whole or partial. |
| `method` | string | sí | Brewing method, such as V60, Chemex, AeroPress, French Press or Espresso. |

#### `list_coffees`

List every coffee currently in the shop's catalogue, with its origin, process, roast level, sensory notes, how many days it is off roast, and which brewing methods have a recipe. Use this to see what is actually available before recommending anything.

Sin parámetros.

#### `list_roast_batches`

List the recorded roast batches for one coffee, with the landmarks of each: first crack, drop, development time and development time ratio. Use this before comparing two batches, to find out which labels exist.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `coffee` | string | sí | Coffee name, whole or partial. |

#### `recommend_coffee`

Find the coffees in the shop's catalogue that best match a customer's request, optionally restricted to those with a recipe for a given method. Recommendations come only from what the shop actually has in stock.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `limit` | integer | no | How many suggestions to return. Defaults to 3. |
| `method` | string | no | Restrict to coffees that have a recipe for this method. Optional. |
| `notes` | array&lt;string&gt; | sí | Flavour descriptors the customer asked for, such as floral, fruity, chocolate or bright. |

#### `record_extraction`

Save a brew that was made, so the shop builds a history of what has been tried on each lot. Use this after a barista reports a result they want kept.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `coffee` | string | sí | Coffee name, whole or partial. |
| `dose_grams` | number | sí | Grams of coffee used. |
| `grind_label` | string | no | Grind setting used. |
| `method` | string | sí | Brewing method. |
| `notes` | string | no | What the barista tasted. |
| `rating` | integer | no | How the cup was rated, from 1 to 10. |
| `seconds` | integer | sí | How long the extraction took. |
| `temp_c` | number | no | Water temperature in Celsius. |
| `water_grams` | number | sí | Grams of water used. |

#### `scale_recipe`

Work a recipe out for a specific amount and return a full brew card: dose, water, bloom and the pour schedule with running totals. Give either the water you want to brew or the dose you have; the server derives the other from the shop's ratio. Always use this rather than calculating the dose yourself.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `coffee` | string | sí | Coffee name, whole or partial. |
| `dose_grams` | number | no | Grams of coffee available. Give this or water_grams, not both. |
| `method` | string | sí | Brewing method. |
| `water_grams` | number | no | Grams of water to brew. Give this or dose_grams, not both. |

### 2.3 Ejemplo completo

Petición:

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "scale_recipe",
    "arguments": { "coffee": "Guji", "method": "V60", "water_grams": 350 }
  }
}
```

Respuesta (contenido textual, con saltos de línea reales):

```
Ethiopia Guji Uraga on V60
  dose        21.0 g
  water       350 g
  ratio       1:16.7
  temperature 94 °C
  grind       medium-fine (~650 µm)
  pours
    bloom         41.9 g at 0:00 (total 42 g)
    first pour   154.1 g at 0:45 (total 196 g)
    second pour  154.0 g at 1:15 (total 350 g)
  target time 2:45–3:10
  rest        12 days off roast (peak)
```

El gramaje no lo produce el modelo: se deriva de la proporción 1:16.7 registrada
por el negocio, y las etapas del vertido suman exactamente el total que mostrará
la balanza.

---

## 3. netprobe

Servidor remoto de diagnóstico de red. Publica herramientas que resuelven
nombres, abren conexiones TCP e intercambian peticiones HTTP, reportando lo que
cada operación hizo realmente: las direcciones que un nombre resuelve, el tiempo
que tomó un handshake, la versión de TLS y el cifrado que dos extremos
negociaron.

Al ser el único componente genuinamente remoto del proyecto, lo que observa
constituye evidencia sobre las capas inferiores al protocolo, en lugar de una
descripción de ellas.

### 3.1 Transporte y endpoints

| | |
| --- | --- |
| Transporte | Streamable HTTP (MCP 2025-06-18) |
| Endpoint MCP | `POST /mcp` |
| Salud | `GET /health` |
| Puerto | `$PORT`, o `:8080` |

| Método HTTP | Comportamiento |
| --- | --- |
| `POST /mcp` | Recibe un frame JSON-RPC y responde con otro |
| `DELETE /mcp` | Termina la sesión indicada en `Mcp-Session-Id` |
| `GET /mcp` | `405`. Este servidor no envía mensajes por iniciativa propia, y mantener una conexión abierta para mensajes que nunca llegan no es gratis |

**Cabeceras.** La petición debe declarar ambos tipos aceptables, porque la
respuesta puede ser un frame único o un flujo de eventos:

```
Content-Type: application/json
Accept: application/json, text/event-stream
```

**Sesión.** El servidor emite un identificador de sesión en la respuesta a
`initialize`, y el cliente debe reenviarlo en toda petición posterior:

```
HTTP/1.1 200 OK
Content-Type: application/json
Mcp-Session-Id: 2697786776289542af033b2cd289be7a
```

A partir de `initialize`, el cliente incluye también la versión acordada:

```
Mcp-Session-Id: <identificador emitido>
MCP-Protocol-Version: 2025-06-18
```

**Códigos de estado:**

| Estado | Cuándo |
| --- | --- |
| `200` | Hay una respuesta JSON-RPC en el cuerpo |
| `202` | El frame era una notificación; el cuerpo vacío es deliberado |
| `400` | Frame malformado |

### 3.2 Herramientas

#### `dns_lookup`

Resolve a hostname to its IPv4 and IPv6 addresses and report how long resolution took. Answers the question of what the name layer knows about a host.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `hostname` | string | sí | The hostname to resolve, without a scheme or path. |

#### `http_probe`

Send a HEAD request to a URL and report the status, the response headers, the negotiated TLS version and cipher, and the time taken. Shows what the application and presentation layers agreed on.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `url` | string | sí | An http or https URL. |

#### `tcp_probe`

Open a TCP connection to one or more ports on a host and report which accepted, which refused, and how long the handshake took. Measures reachability at the transport layer.

| Parámetro | Tipo | Obligatorio | Descripción |
| --- | --- | --- | --- |
| `host` | string | sí | The host to connect to. |
| `ports` | array&lt;integer&gt; | sí | The TCP ports to try. |

### 3.3 Ejemplo completo

Handshake real capturado del servidor:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "capabilities": {
      "tools": {}
    },
    "protocolVersion": "2025-06-18",
    "serverInfo": {
      "name": "netprobe",
      "version": "1.0.0"
    }
  }
}
```

Invocación de una herramienta:

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "dns_lookup",
    "arguments": { "hostname": "github.com" }
  }
}
```

Contenido de la respuesta:

```
github.com resolved in 132ms
  IPv4: 140.82.112.4
  IPv6: (none)
```

### 3.4 Restricciones de seguridad

Una herramienta que realiza peticiones de red por cuenta de quien la invoca es
una puerta hacia todo lo que el servidor alcanza. Desplegado en la nube, eso
incluye el servicio de metadatos del proveedor y el resto de la red privada.

Antes de cualquier operación, el destino se resuelve y se rechaza si **alguna**
de sus direcciones cae en un rango interno:

| Rango | Ejemplo | Motivo |
| --- | --- | --- |
| Loopback | `127.0.0.1`, `::1` | El propio servidor |
| Privado | `10.0.0.0/8`, `192.168.0.0/16`, `172.16.0.0/12` | Red interna |
| Link-local | `169.254.169.254` | Metadatos de nube |
| No especificado | `0.0.0.0` | — |

También se rechaza `localhost` por nombre y todo esquema que no sea `http` o
`https`.

---

## 4. Resumen de interoperabilidad

El anfitrión desarrollado en este proyecto conecta simultáneamente servidores
escritos en tres lenguajes distintos, por dos empresas distintas, sobre dos
transportes distintos:

| Servidor | Lenguaje | Origen | Transporte |
| --- | --- | --- | --- |
| `filesystem` | TypeScript | Anthropic (npm) | stdio |
| `git` | Python | Anthropic (PyPI) | stdio |
| `brewops` | Go | propio | stdio |
| `netprobe` | Go | propio | HTTP |

El anfitrión no adapta nada para ninguno de ellos. Esa es exactamente la
afirmación con la que el enunciado abre: que MCP hace que el desarrollo de la
herramienta sea independiente del modelo que la consume.
