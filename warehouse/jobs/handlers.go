/*
	Warehouse jobs package provides the capability to running arbitrary jobs on the warehouses using the query parameters provided.
	Some of the jobs that can be run are
	1) delete by task run id,
	2) delete by job run id,
	3) delete by update_at
	4) any other update / clean up operations

	The following handlers file is the entry point for the handlers.
*/

package jobs

import (
	"encoding/json"
	"io"
	"net/http"
)

// The following handler gets called for adding async
func (asyncWhJob *AsyncJobWhT) AddWarehouseJobHandler(w http.ResponseWriter, r *http.Request) {
	pkgLogger.Info("[WH-Jobs] Got Async Job Add Request")
	pkgLogger.LogRequest(r)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		pkgLogger.Errorf("[WH-Jobs]: Error reading body: %v", err)
		http.Error(w, "can't read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	var startJobPayload StartJobReqPayload
	err = json.Unmarshal(body, &startJobPayload)
	if err != nil {
		pkgLogger.Errorf("[WH-Jobs]: Error unmarshalling body: %v", err)
		http.Error(w, "can't unmarshall body", http.StatusBadRequest)
		return
	}
	if !validatePayload(startJobPayload) {
		pkgLogger.Errorf("[WH-Jobs]: Invalid Payload %v", err)
		http.Error(w, "invalid Payload", http.StatusBadRequest)
		return
	}
	if !asyncWhJob.enabled {
		pkgLogger.Errorf("[WH-Jobs]: Error Warehouse Jobs API not initialized %v", err)
		http.Error(w, "warehouse jobs api not initialized", http.StatusBadRequest)
		return
	}
	tableNames, err := asyncWhJob.getTableNamesBy(startJobPayload.SourceID, startJobPayload.DestinationID, startJobPayload.JobRunID, startJobPayload.TaskRunID)
	if err != nil {
		pkgLogger.Errorf("[WH-Jobs]: Error extracting tableNames for the job run id: %v", err)
		http.Error(w, "Error extracting tableNames", http.StatusBadRequest)
		return
	}
	var jobIds []int64
	// Add to wh_async_job queue each of the tables
	for _, th := range tableNames {
		if !skipTable(th) {
			whmetadata := WhJobsMetaData{
				JobRunID:  startJobPayload.JobRunID,
				TaskRunID: startJobPayload.TaskRunID,
				StartTime: startJobPayload.StartTime,
				JobType:   AsyncJobType,
			}
			metadata, err := json.Marshal(whmetadata)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			payload := AsyncJobPayloadT{
				SourceID:      startJobPayload.SourceID,
				DestinationID: startJobPayload.DestinationID,
				TableName:     th,
				AsyncJobType:  startJobPayload.AsyncJobType,
				MetaData:      metadata,
			}
			id, err := asyncWhJob.addJobstoDB(asyncWhJob.context, &payload)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			jobIds = append(jobIds, id)
		}
	}
	whAddJobResponse := WhAddJobResponse{
		JobIds: jobIds,
		Err:    nil,
	}
	response, err := json.Marshal(whAddJobResponse)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, _ = w.Write(response)
}

func (asyncWhJob *AsyncJobWhT) StatusWarehouseJobHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		pkgLogger.Info("Got Async Job Status Request")
		pkgLogger.LogRequest(r)
		jobRunId := r.URL.Query().Get("job_run_id")
		taskRunId := r.URL.Query().Get("task_run_id")

		sourceId := r.URL.Query().Get("source_id")
		destinationId := r.URL.Query().Get("destination_id")
		payload := StartJobReqPayload{
			TaskRunID:     taskRunId,
			JobRunID:      jobRunId,
			SourceID:      sourceId,
			DestinationID: destinationId,
		}
		if !validatePayload(payload) {

			pkgLogger.Errorf("[WH]: Error Invalid Status Parameters")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		startJobPayload := StartJobReqPayload{
			JobRunID:      jobRunId,
			TaskRunID:     taskRunId,
			SourceID:      sourceId,
			DestinationID: destinationId,
		}
		pkgLogger.Infof("Got Payload job_run_id %s, task_run_id %s \n", startJobPayload.JobRunID, startJobPayload.TaskRunID)

		if !asyncWhJob.enabled {
			pkgLogger.Errorf("[WH]: Error Warehouse Jobs API not initialized")
			http.Error(w, "warehouse jobs api not initialized", http.StatusBadRequest)
			return
		}

		response := asyncWhJob.getStatusAsyncJob(asyncWhJob.context, &startJobPayload)

		writeResponse, err := json.Marshal(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Write(writeResponse)
	} else {
		pkgLogger.Errorf("[WH]: Error Invalid Method")
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
}
