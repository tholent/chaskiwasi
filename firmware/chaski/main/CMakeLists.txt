idf_component_register(SRCS "app_main.cpp" "chaski_strings.c"
                       INCLUDE_DIRS "."
                       REQUIRES wire store ayllu layout syncengine transport
                                panel input power pututu kipu ui modem
                                nvs_flash spiffs esp_timer)
