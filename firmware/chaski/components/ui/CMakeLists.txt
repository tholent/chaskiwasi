# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
#
# main/ appears as a PRIVATE include path, and only for the string-id enum
# (F-C12). The screens resolve text through an injected function pointer, so no
# symbol defined in main/ is referenced from here and the link graph stays
# one-directional — an IDF component cannot require main, and reversing that
# edge would be the real dependency error.
set(CHASKI_UI_SRCS ui.cpp cover.cpp text_util.cpp)

if(ESP_PLATFORM)
  idf_component_register(SRCS ${CHASKI_UI_SRCS}
                         INCLUDE_DIRS "include"
                         PRIV_INCLUDE_DIRS "../../main"
                         REQUIRES wire store ayllu layout panel input kipu)
else()
  add_library(chaski_ui STATIC ${CHASKI_UI_SRCS})
  target_include_directories(chaski_ui PUBLIC include)
  target_include_directories(chaski_ui PRIVATE ${CMAKE_CURRENT_LIST_DIR}/../../main)
  target_compile_features(chaski_ui PUBLIC cxx_std_17)
  target_link_libraries(chaski_ui PUBLIC chaski_wire chaski_store chaski_ayllu
                                         chaski_layout chaski_panel chaski_input
                                         chaski_kipu)
endif()
