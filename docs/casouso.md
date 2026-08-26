BrewOps — Asistente GenAI para cafeterías y tostadores de café de especialidad
Descripción del caso de uso

BrewOps es un asistente de GenAI orientado a cafeterías y tostadores de café de especialidad, diseñado para ayudar a baristas y personal de producción a preparar café de manera consistente según las características de cada grano y el método de preparación utilizado.

El sistema almacenará información propia del negocio sobre los cafés disponibles, como origen, variedad, proceso, fecha y nivel de tueste, notas sensoriales y recetas recomendadas. A partir de estos datos, el asistente podrá calcular proporciones de café y agua, adaptar recetas a diferentes cantidades, recomendar parámetros de preparación y analizar resultados de extracciones.

También podrá utilizar información de los perfiles de tueste para comparar distintos lotes de un mismo café y detectar cambios relevantes que puedan explicar diferencias en el resultado final de la bebida.

El objetivo es centralizar el conocimiento de preparación y tueste de la cafetería, facilitar el entrenamiento de baristas y mejorar la consistencia del producto entre distintas preparaciones o sucursales.
Cómo se utilizaría su servidor

El barista o tostador interactuará con el asistente mediante un chatbot. Para el desarrollo del proyecto, la interacción se realizará desde la consola.

El LLM interpretará las solicitudes del usuario y utilizará las herramientas proporcionadas por el servidor MCP para consultar los cafés y recetas registradas, realizar cálculos y analizar información relacionada con las preparaciones y perfiles de tueste.

Por ejemplo, en lugar de que el modelo invente una receta, podrá consultar la receta registrada por la cafetería para un café específico y posteriormente ajustarla según el método o la cantidad que el barista desea preparar.

El servidor podrá manejar información como:

    cafés disponibles y sus características;
    métodos de preparación;
    recetas base;
    proporciones de café y agua;
    temperatura y molienda recomendadas;
    tiempo esperado de extracción;
    resultados de preparaciones anteriores;
    información de lotes y perfiles de tueste.

Casos de uso relevantes

Caso de uso 1: Generación y adaptación de una receta

Un barista necesita preparar 350 gramos de un café Ethiopia Guji utilizando V60.

El asistente consulta la información y receta registrada para ese café y método. El servidor calcula el gramaje de café necesario según la proporción establecida y adapta los demás parámetros de la receta.

Por ejemplo, podría indicar una preparación de 21 gramos de café para 350 gramos de agua utilizando una proporción aproximada de 1:16.7, junto con la temperatura, molienda, bloom y tiempo de extracción registrados para ese café.

De esta manera, el barista no necesita realizar los cálculos manualmente y la preparación se mantiene consistente.

Caso de uso 2: Ajuste de una extracción

El barista prepara un V60 utilizando una receta registrada, pero la extracción termina en 2 minutos y 10 segundos cuando el rango esperado es aproximadamente de 2 minutos y 45 segundos a 3 minutos y 10 segundos.

El usuario informa el resultado al asistente.

El servidor compara los parámetros reales de la preparación contra la receta esperada e identifica las variables fuera de rango. El asistente puede recomendar, por ejemplo, probar una molienda más fina manteniendo inicialmente las demás variables constantes.

El objetivo es ayudar al barista a corregir una preparación basándose en los parámetros registrados por la cafetería y no únicamente en conocimiento general del LLM.

Caso de uso 3: Recomendación de un café según el cliente y método

Un cliente solicita un café floral, con acidez pronunciada y que sea apropiado para prepararse en V60.

El barista consulta al asistente.

El servidor revisa el catálogo actual de cafés de la cafetería, sus notas sensoriales, procesamiento y recetas disponibles, y devuelve las opciones que mejor coinciden con las características solicitadas.

La recomendación se realiza únicamente entre los cafés disponibles en el negocio y puede incluir la receta recomendada para preparar la opción seleccionada.

Caso de uso 4: Comparación entre lotes de tueste

El tostador observa que un nuevo lote de un mismo café está produciendo un resultado diferente al lote anterior.

El asistente consulta los perfiles registrados de ambos tuestes y utiliza el servidor MCP para comparar variables como duración total, momento del primer crack, tiempo de desarrollo y comportamiento de la temperatura durante el proceso.

El servidor identifica los puntos donde ambos perfiles presentan diferencias significativas y el asistente los resume para facilitar el análisis del tostador.

Esto permite relacionar diferencias entre lotes de tueste con cambios observados posteriormente durante la preparación del café.
