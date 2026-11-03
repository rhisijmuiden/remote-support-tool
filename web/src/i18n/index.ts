import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import en from "./en.json";
import nl from "./nl.json";

// English is the default; Dutch is a full translation. The choice is stored per user on the server.
function initialLanguage(): string {
  try {
    const saved = localStorage.getItem("lang");
    if (saved === "en" || saved === "nl") return saved;
  } catch {
    /* storage unavailable */
  }
  return navigator.language?.toLowerCase().startsWith("nl") ? "nl" : "en";
}

i18n.use(initReactI18next).init({
  resources: { en: { translation: en }, nl: { translation: nl } },
  lng: initialLanguage(),
  fallbackLng: "en",
  interpolation: { escapeValue: false },
});

export function setLanguage(lang: "en" | "nl") {
  i18n.changeLanguage(lang);
  document.documentElement.lang = lang;
  try {
    localStorage.setItem("lang", lang);
  } catch {
    /* ignore */
  }
}

export default i18n;
