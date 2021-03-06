package dispatcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gofort/dispatcher/utils"
	"github.com/streadway/amqp"
	"reflect"
	"runtime/debug"
	"sync"
	"time"
)

// WorkerConfig is a configuration for new worker which you want to create.
//
// Limit - number of parallel tasks which will be executed.
//
// Queue - name of queue which worker will consume.
//
// Binding keys - biding keys for queue which will be created.
//
// Name - worker name
type WorkerConfig struct {
	Limit       int
	Queue       string // required
	BindingKeys []string
	Name        string // required
}

// TaskConfig is task configuration which is needed for task registration in worker.
// Contains function which will be called by worker and timeout.
// Timeout is needed in case your task executing for about half an hour but you expected only 1 minute.
// When timeout exceeded next task will be taken, but that old task will not be stopped.
// TaskUUIDAsFirstArg - makes task UUID as first argument of all tasks which this worker calls.
type TaskConfig struct {
	TimeoutSeconds     int64
	Function           interface{}
	TaskUUIDAsFirstArg bool
}

// Worker instance.
// Consists of channel which consume queue.
type Worker struct {
	log Log // logger, which was taken from server instance

	ch               *amqp.Channel        // channel which is used for messages consuming
	stopConsume      chan struct{}        // channel which is used to stop consuming process
	consumingStopped chan struct{}        // channel which notifies that consuming stopped
	deliveries       <-chan amqp.Delivery // deliveries which worker is receiving
	tasksInProgress  *sync.WaitGroup      // wait group for waiting all tasks finishing when we close this worker

	queue string // queue name which will be subscribed by this worker
	name  string // worker name, also used as consumer tag
	limit int    // number of tasks which can be executed in parallel

	tasks map[string]TaskConfig // tasks configurations, to know their timeouts and know if this worker should execute task

	working bool // indicates if worker was started earlier
}

// NewWorker creates new worker instance.
// Takes WorkerConfig and map of TaskConfigs.
// Map of TaskConfigs needs for task registration inside of this worker.
func (s *Server) NewWorker(cfg *WorkerConfig, tasks map[string]TaskConfig) (*Worker, error) {

	if !s.con.connected {
		return nil, errors.New("Can't create new worker, because you are not connected to AMQP")
	}

	if cfg.Name == "" {
		return nil, errors.New("Worker name is required parameter")
	}

	if cfg.Limit == 0 {
		cfg.Limit = 3
	}

	if cfg.Queue == "" {
		return nil, errors.New("Worker queue is required parameter")
	}

	if _, ok := s.workers[cfg.Name]; ok {
		return nil, errors.New("Worker with the same name already exists")
	}

	w := &Worker{
		name:            cfg.Name,
		log:             s.log,
		tasks:           tasks,
		limit:           cfg.Limit,
		queue:           cfg.Queue,
		tasksInProgress: new(sync.WaitGroup),
	}

	var err error

	w.ch, err = s.con.con.Channel()
	if err != nil {
		s.log.Errorf("Error during creating channel: %v", err)
		return nil, err
	}
	defer w.ch.Close()

	err = declareQueue(w.ch, cfg.Queue)
	if err != nil {
		s.log.Errorf("Error during declaring queue: %v", err)
		return nil, err
	}

	for _, k := range cfg.BindingKeys {

		err = queueBind(w.ch, s.publisher.defaultExchange, cfg.Queue, k)
		if err != nil {
			s.log.Errorf("Error during binding queue: %v", err)
			return nil, err
		}

	}

	s.workers[cfg.Name] = w

	return w, nil

}

// Start function starts consuming of queue.
// Needs server as an argument because only server contains AMQP connection and this function creates AMQP channel
// for a worker from connection.
func (w *Worker) Start(s *Server) error {

	if !s.con.connected {
		return errors.New("Can't start worker, because you are not connected to AMQP")
	}

	return w.init(s.con.con)

}

func (w *Worker) init(con *amqp.Connection) error {

	w.working = true

	w.stopConsume = make(chan struct{})
	w.consumingStopped = make(chan struct{})

	var err error

	w.ch, err = con.Channel()
	if err != nil {
		return fmt.Errorf("Error during creating channel for worker: %v", err)
	}

	if err := w.ch.Qos(
		w.limit, // prefetch count
		0,       // prefetch size
		false,   // global
	); err != nil {
		return fmt.Errorf("Error during setting QoS for worker's channel: %v", err)
	}

	w.deliveries, err = w.ch.Consume(
		w.queue, // queue
		w.name,  // consumer tag
		false,   // auto-ack
		false,   // exclusive
		false,   // no-local
		false,   // no-wait
		nil,     // arguments
	)
	if err != nil {
		return fmt.Errorf("Error during initialization queue consuming: %v", err)
	}

	go w.consume(w.deliveries)

	return nil

}

func (w *Worker) consume(deliveries <-chan amqp.Delivery) {

	w.log.Infof("Worker %s started consuming", w.name)

	for {
		select {

		case <-w.stopConsume:

			w.log.Debug("Consuming stopped")

			w.consumingStopped <- struct{}{}

			return

		case d := <-deliveries:

			if len(d.Body) == 0 {

				w.log.Error("Empty task received")

				if er := d.Nack(false, false); er != nil {
					w.log.Errorf("Consuming stopped: %v", er)
					return
				}

				continue
			}

			var task Task
			if err := json.Unmarshal(d.Body, &task); err != nil {

				if er := d.Nack(false, false); er != nil {
					w.log.Errorf("Consuming stopped: %v", er)
					return
				}

				w.log.Errorf("%v, task body: %s", errors.New("Can't unmarshal received task"), string(d.Body))
				continue
			}

			taskConfig, ok := w.tasks[task.Name]
			if !ok {

				if er := d.Nack(false, true); er != nil {
					w.log.Errorf("Consuming stopped: %v", er)
					return
				}

				w.log.Errorf("Received task (%s-%s) which is not registered in this worker, task was requeued, but somebody should take it from this queue in other case error will be retried", task.Name, task.UUID)
				continue
			}

			w.tasksInProgress.Add(1)

			go w.consumeOne(d, task, taskConfig)

		}
	}

}

// Close function gracefully closes worker.
// At first this function stops worker consuming, then waits until all started by this worker tasks will be finished
// after all of this it closes channel.
// This function is also used by server close function for graceful quit of all workers.
func (w *Worker) Close() {

	w.log.Debugf("Worker %s closing started", w.name)

	if !w.working {
		return
	}

	w.working = false

	w.stopConsume <- struct{}{}
	close(w.stopConsume)

	<-w.consumingStopped
	close(w.consumingStopped)

	w.tasksInProgress.Wait()

	w.ch.Close()

	w.log.Infof("Worker %s is closed", w.name)

}

func (w *Worker) consumeOne(d amqp.Delivery, task Task, taskConfig TaskConfig) {
	defer w.tasksInProgress.Done()

	var err error

	w.log.Infof("Handling task %s", task.UUID)

	reflectedTaskFunction := reflect.ValueOf(taskConfig.Function)

	if taskConfig.TaskUUIDAsFirstArg {

		taskUUID := []TaskArgument{{"string", task.UUID}}

		task.Args = append(taskUUID, task.Args...)

	}

	reflectedTaskArgs, err := reflectArgs(task.Args)
	if err != nil {
		d.Nack(false, false)
		w.log.Errorf("Can't reflect task (%s) arguments: %v", task.UUID, err)
		return
	}

	timeouted := tryCall(reflectedTaskFunction, reflectedTaskArgs, taskConfig.TimeoutSeconds)
	if timeouted {
		w.log.Infof("Task %s exceeded timeout, taking next task", task.UUID)
	} else {
		w.log.Infof("Task %s was finished", task.UUID)
	}

	d.Ack(false)

}

func reflectArgs(args []TaskArgument) ([]reflect.Value, error) {
	argValues := make([]reflect.Value, len(args))

	for i, arg := range args {
		argValue, err := utils.ReflectValue(arg.Type, arg.Value)
		if err != nil {
			return nil, err
		}
		argValues[i] = argValue
	}

	return argValues, nil
}

func tryCall(f reflect.Value, args []reflect.Value, timeoutSeconds int64) (finishedByTimeout bool) {

	defer func() {
		if e := recover(); e != nil {
			fmt.Printf("%s", debug.Stack())
		}
	}()

	if timeoutSeconds == 0 {
		f.Call(args)
		return false
	}

	timer := time.NewTimer(time.Second * time.Duration(timeoutSeconds))
	resultsChan := make(chan []reflect.Value)

	go func() {
		resultsChan <- f.Call(args)
	}()

	select {
	case <-timer.C:
		return true
	case <-resultsChan:

	}

	return false
}
