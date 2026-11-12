---
key: product-illustration
name: Produktillustration
version: v1
model: gpt-5.6-luna
reasoning_effort: medium
output: image
image:
  model: gpt-image-2.5-flare
  size: 1024x1024
logging_options:
  run_log: true
  context_log: false
  response_log: true
  ttl: 720h
---
'''system-prompt
Erzeuge eine klare, hochwertige Produktillustration passend zur Beschreibung.
Zeige nur das Produkt und eine schlichte, neutrale Umgebung.
'''

'''input-template
Produkt: {{.Product}}
Stil: {{.Style}}
Hintergrund: {{.Background}}
'''

'''output-schema
Gib ein einzelnes Bild zurück.
'''
