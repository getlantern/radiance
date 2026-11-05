package ipc

import "net/http"

func (s *localapi) accountBindLegacyEmailHandler(w http.ResponseWriter, r *http.Request) {
	var err error
	switch r.PathValue("action") {
	case "start":
		var req EmailRequest
		if err = decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = s.backend(r.Context()).StartBindLegacyEmail(r.Context(), req.Email)
	case "complete":
		var req BindLegacyEmailCompleteRequest
		if err = decodeJSON(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = s.backend(r.Context()).CompleteBindLegacyEmail(r.Context(), req.Email, req.Password, req.Code)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
