# Vendored utf8proc 2.8.0 (Unicode 15.0.0) — JuliaStrings/utf8proc, MIT.
#
# Vendored rather than depended on, for two reasons.
#
# 1. ESP-IDF does not ship it and the component registry has no port, so the
#    target build has no other source (F-C8).
# 2. Parity. The host tier previously linked Debian's libutf8proc while the
#    device would have linked something else — reintroducing exactly the
#    two-implementations skew that decision B.7 and test C-9 exist to prevent.
#    Both platforms now compile THIS source, so C-9 validates the code the
#    device actually runs.
#
# The pinned version must stay in lockstep with the server's rivo/uniseg
# Unicode version; see firmware/chaski/README.md. Do not upgrade one alone.
#
# Only the grapheme-segmentation surface is used. UTF8PROC_STATIC keeps the
# symbols internal — nothing here is part of the firmware's public API.
if(ESP_PLATFORM)
  idf_component_register(SRCS "utf8proc.c"
                         INCLUDE_DIRS ".")
  target_compile_definitions(${COMPONENT_LIB} PUBLIC UTF8PROC_STATIC)
else()
  add_library(utf8proc STATIC utf8proc.c)
  target_include_directories(utf8proc PUBLIC ${CMAKE_CURRENT_LIST_DIR})
  target_compile_definitions(utf8proc PUBLIC UTF8PROC_STATIC)
  # Vendor code is not ours to lint; our own warnings stay strict.
  target_compile_options(utf8proc PRIVATE -w)
endif()
